// A typed Drive API client for the dockerized-Drive smoke suite. It ports
// the login/create/upload/poll recipe e2e/scripts/seed.mjs already proved
// against the real Drive stack, instead of shelling out to it: a
// Playwright test needs two independent logged-in users (a writer and a
// read-only sharee) alive at the same time, in the same process, which a
// one-shot CLI script cannot give it.
//
// This file must import only node:crypto, never @playwright/test: a
// vitest suite imports DriveClient directly, outside any Playwright
// runner.

import crypto from 'node:crypto'

const DRIVE_URL = process.env.DRIVE_URL ?? 'http://localhost:8000'

const WOPI_POLL_INTERVAL_MS = 1000
const WOPI_POLL_TIMEOUT_MS = 60_000

// S3 (rustfs) settings for downloadFileContent, matching e2e/env/drive.env's
// AWS_S3_* values. This stack's Drive backend has no working content-download
// route (see downloadFileContent's own comment), so the test signs its own
// GET straight against the object store Drive itself writes to.
const S3_ENDPOINT = process.env.DRIVE_S3_ENDPOINT_URL ?? 'http://localhost:9000'
const S3_BUCKET = process.env.DRIVE_S3_BUCKET ?? 'drive-media-storage'
const S3_REGION = process.env.DRIVE_S3_REGION ?? 'eu-east-1'
const S3_ACCESS_KEY_ID = process.env.DRIVE_S3_ACCESS_KEY_ID ?? 'drive'
const S3_SECRET_ACCESS_KEY = process.env.DRIVE_S3_SECRET_ACCESS_KEY ?? 'password'
const S3_PRESIGN_EXPIRY_SECONDS = 60

function hmac(key: crypto.BinaryLike, msg: string): Buffer {
  return crypto.createHmac('sha256', key).update(msg, 'utf8').digest()
}

function sha256hex(msg: string): string {
  return crypto.createHash('sha256').update(msg, 'utf8').digest('hex')
}

// s3SigningKey derives the SigV4 signing key (AWS's own date/region/service/
// request key-derivation chain).
function s3SigningKey(secretKey: string, dateStamp: string, region: string): Buffer {
  const kDate = hmac('AWS4' + secretKey, dateStamp)
  const kRegion = hmac(kDate, region)
  const kService = hmac(kRegion, 's3')
  return hmac(kService, 'aws4_request')
}

// awsUriEncode applies SigV4's URI-encoding rule (AWS docs, "URI Encode"):
// every byte percent-encoded except A-Z a-z 0-9 - _ . ~. encodeURIComponent
// leaves four extra characters unencoded (! * ' ( )), so this closes that
// gap before the value goes into a canonical request.
function awsUriEncode(value: string): string {
  return encodeURIComponent(value).replace(/[!*'()]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)
}

// presignGetUrl signs a GET for key in S3_BUCKET, the same SigV4
// query-string scheme Drive's own presigned upload PUT (item.policy) uses,
// so DriveClient needs no S3 SDK dependency for this test-only read path.
function presignGetUrl(key: string): string {
  const host = new URL(S3_ENDPOINT).host
  const now = new Date()
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, '')
  const dateStamp = amzDate.slice(0, 8)
  const credentialScope = `${dateStamp}/${S3_REGION}/s3/aws4_request`
  const canonicalUri = `/${S3_BUCKET}/${key.split('/').map(awsUriEncode).join('/')}`
  const params: Record<string, string> = {
    'X-Amz-Algorithm': 'AWS4-HMAC-SHA256',
    'X-Amz-Credential': `${S3_ACCESS_KEY_ID}/${credentialScope}`,
    'X-Amz-Date': amzDate,
    'X-Amz-Expires': String(S3_PRESIGN_EXPIRY_SECONDS),
    'X-Amz-SignedHeaders': 'host',
  }
  const canonicalQuerystring = Object.keys(params).sort()
    .map(k => `${encodeURIComponent(k)}=${encodeURIComponent(params[k])}`).join('&')
  const canonicalRequest = ['GET', canonicalUri, canonicalQuerystring, `host:${host}\n`, 'host', 'UNSIGNED-PAYLOAD'].join('\n')
  const stringToSign = ['AWS4-HMAC-SHA256', amzDate, credentialScope, sha256hex(canonicalRequest)].join('\n')
  const signature = crypto.createHmac('sha256', s3SigningKey(S3_SECRET_ACCESS_KEY, dateStamp, S3_REGION))
    .update(stringToSign, 'utf8').digest('hex')
  return `${S3_ENDPOINT}${canonicalUri}?${canonicalQuerystring}&X-Amz-Signature=${signature}`
}

// randomCsrfToken mints the client half of Django's CSRF double-submit
// check: SessionAuthentication.enforce_csrf only requires the
// X-CSRFToken header to match whatever csrftoken cookie the client holds,
// so a fresh random value works as well as one Django itself minted (see
// e2e/scripts/seed.mjs's identical recipe).
function randomCsrfToken(): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
  let token = ''
  for (let i = 0; i < 64; i++) {
    token += alphabet[Math.floor(Math.random() * alphabet.length)]
  }
  return token
}

class CookieJar {
  #cookies = new Map<string, string>()

  absorb(response: Response): void {
    for (const setCookie of response.headers.getSetCookie?.() ?? []) {
      const [pair] = setCookie.split(';')
      const eq = pair.indexOf('=')
      if (eq === -1) continue
      this.#cookies.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim())
    }
  }

  set(name: string, value: string): void {
    this.#cookies.set(name, value)
  }

  get(name: string): string | undefined {
    return this.#cookies.get(name)
  }

  header(): string {
    return [...this.#cookies.entries()].map(([k, v]) => `${k}=${v}`).join('; ')
  }
}

export interface DriveItem {
  id: string
  policy: string
  filename: string
}

export interface WopiLaunch {
  launchUrl: string
  accessToken: string
  accessTokenTtl: number
}

export interface ItemDetail {
  id: string
  updatedAt: string
  size: number | null
}

// DriveClient is one logged-in Drive session: one user, one cookie jar.
// Build a separate instance per test user; CookieJars never share state,
// so two clients act as two independent browser profiles against the
// same Drive.
export class DriveClient {
  #jar = new CookieJar()

  async #api(method: string, path: string, opts: { body?: unknown, raw?: boolean } = {}): Promise<any> {
    const url = path.startsWith('http') ? path : `${DRIVE_URL}${path}`
    const csrf = this.#jar.get('csrftoken')
    const headers: Record<string, string> = {
      Cookie: this.#jar.header(),
      ...(csrf ? { 'X-CSRFToken': csrf, Referer: DRIVE_URL } : {}),
    }
    if (opts.body !== undefined && !opts.raw) {
      headers['Content-Type'] = 'application/json'
    }

    const response = await fetch(url, {
      method,
      headers,
      body: opts.body === undefined ? undefined : opts.raw ? (opts.body as BodyInit) : JSON.stringify(opts.body),
    })
    this.#jar.absorb(response)

    const text = await response.text()
    let data: unknown = text
    try {
      data = text ? JSON.parse(text) : null
    } catch {
      // Not JSON (e.g. an S3 error body); keep the raw text.
    }

    if (!response.ok) {
      throw new Error(`${method} ${url} -> ${response.status}: ${text}`)
    }
    return data
  }

  // login authenticates through the e2e auth bypass
  // (POST /api/v1.0/e2e/user-auth/, e2e/viewsets.py's UserAuthViewSet),
  // which creates-or-fetches a user by email. Every subsequent call on
  // this client carries the resulting session cookie.
  async login(email: string): Promise<void> {
    await this.#api('POST', '/api/v1.0/e2e/user-auth/', { body: { email } })
    if (!this.#jar.get('csrftoken')) {
      this.#jar.set('csrftoken', randomCsrfToken())
    }
  }

  // me reads the logged-in user's own id, needed to grant them access to
  // an item created by a different DriveClient (shareReadOnly's user_id).
  async me(): Promise<{ id: string, email: string }> {
    return this.#api('GET', '/api/v1.0/users/me/')
  }

  async createItem(filename: string): Promise<DriveItem> {
    const item = await this.#api('POST', '/api/v1.0/items/', { body: { type: 'file', filename } })
    return { id: item.id, policy: item.policy, filename: item.filename }
  }

  // uploadScene PUTs body to item's presigned S3 URL, then tells Drive
  // the upload ended, exactly like e2e/scripts/seed.mjs's uploadScene.
  async uploadScene(item: DriveItem, body: string): Promise<void> {
    const putResponse = await fetch(item.policy, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', 'x-amz-acl': 'private' },
      body,
    })
    if (!putResponse.ok) {
      throw new Error(`PUT ${item.policy} -> ${putResponse.status}: ${await putResponse.text()}`)
    }
    await this.#api('POST', `/api/v1.0/items/${item.id}/upload-ended/`, { body: {} })
  }

  // overwriteScene PUTs body to item's presigned S3 URL only, with no
  // upload-ended call: Drive's upload_ended demands upload_state ==
  // PENDING and answers 400 after the item's first upload, but the raw
  // PUT alone still changes the S3 ETag and so the WOPI version, which
  // is all an out-of-band-drift test needs.
  async overwriteScene(item: DriveItem, body: string): Promise<void> {
    const putResponse = await fetch(item.policy, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', 'x-amz-acl': 'private' },
      body,
    })
    if (!putResponse.ok) {
      throw new Error(`PUT ${item.policy} -> ${putResponse.status}: ${await putResponse.text()}`)
    }
  }

  // wopiLaunch polls GET /api/v1.0/items/{id}/wopi/ until Drive returns a
  // launch payload, the same wait e2e/scripts/seed.mjs needs while the
  // celery-ingested discovery route is not live yet. Every call mints a
  // fresh access token, so a test can open more than one session for the
  // same user against the same item.
  async wopiLaunch(itemId: string): Promise<WopiLaunch> {
    const deadline = Date.now() + WOPI_POLL_TIMEOUT_MS
    let lastError: unknown
    while (Date.now() < deadline) {
      try {
        const wopi = await this.#api('GET', `/api/v1.0/items/${itemId}/wopi/`)
        return { launchUrl: wopi.launch_url, accessToken: wopi.access_token, accessTokenTtl: wopi.access_token_ttl }
      } catch (err) {
        lastError = err
        await new Promise(resolve => setTimeout(resolve, WOPI_POLL_INTERVAL_MS))
      }
    }
    throw new Error(`drive: timed out waiting for /wopi/ to route ${itemId} (last error: ${lastError})`)
  }

  // shareReadOnly grants userId read-only access to itemId through
  // Drive's direct accesses grant (core/api/viewsets.py's
  // ItemAccessViewSet, POST /api/v1.0/items/{id}/accesses/) rather than
  // the by-email invitations flow: the caller already knows the target
  // user's id from a prior DriveClient.me() call, and a direct grant
  // needs no accept step. Body field names and the "reader" role value
  // match every fixture in
  // core/tests/items/test_api_item_accesses_create.py. The caller (this
  // client) must already hold owner/admin on itemId, true by default
  // since DriveClient.createItem makes its own user the creator.
  async shareReadOnly(itemId: string, userId: string): Promise<void> {
    await this.#api('POST', `/api/v1.0/items/${itemId}/accesses/`, { body: { user_id: userId, role: 'reader' } })
  }

  // shareEditor grants userId write access to itemId; see shareReadOnly
  // for the route and precondition rationale, which applies here too.
  async shareEditor(itemId: string, userId: string): Promise<void> {
    await this.#api('POST', `/api/v1.0/items/${itemId}/accesses/`, { body: { user_id: userId, role: 'editor' } })
  }

  // itemDetail reads plain, session-authenticated item metadata --
  // unlike WOPI CheckFileInfo, it needs no X-WOPI-Proof signature. This
  // is the smoke suite's save-landed signal: wopi/viewsets.py's
  // _put_file_content saves item.size and item.updated_at on every WOPI
  // PutFile, and Drive's WopiViewSet._verify_request_signature (wopi/
  // viewsets.py:74-114) always demands a valid signature for a raw
  // CheckFileInfo call, since our discovery XML always publishes a
  // proof-key: an unsigned GET .../wopi/files/{id} raises an uncaught
  // WopiRequestSignatureError, surfacing as 500. Only our own Go server
  // holds the proof private key, and it does not expose a public "run
  // CheckFileInfo for me" route, so this item-detail path is the one a
  // test can actually call.
  async itemDetail(itemId: string): Promise<ItemDetail> {
    const item = await this.#api('GET', `/api/v1.0/items/${itemId}/`)
    return { id: item.id, updatedAt: item.updated_at, size: item.size ?? null }
  }

  // downloadFileContent reads item's saved file bytes straight from the
  // object store, bypassing Drive's own content-download routes: this
  // dev/e2e stack runs no nginx (see compose.yaml's host-networking note),
  // and Drive's `GET /items/{id}/download/` 302s to
  // `{MEDIA_BASE_URL}{MEDIA_URL}{file_key}`, which in production only nginx's
  // media-auth subrequest resolves against S3 -- here it falls through to
  // Django's own `static()` fallback route instead, and that route 500s
  // outright (`serve() got an unexpected keyword argument 'item_root'`,
  // confirmed against the live stack), because Django's built-in static
  // view only ever serves a local filesystem document_root, never S3, so it
  // could not serve S3-backed content even with that argument fixed. Drive
  // itself writes every WOPI PutFile to `item.file_key` in
  // core/models.py (`item/{itemId}/{filename}`, wopi/viewsets.py's
  // `_put_file_content`), so a self-signed SigV4 GET against that same key
  // (presignGetUrl) reads the exact bytes Drive's own save path wrote,
  // using the same AWS_S3_* credentials e2e/env/drive.env already carries.
  async downloadFileContent(item: DriveItem): Promise<string> {
    const url = presignGetUrl(`item/${item.id}/${item.filename}`)
    const response = await fetch(url)
    if (!response.ok) {
      throw new Error(`GET ${url} -> ${response.status}: ${await response.text()}`)
    }
    return response.text()
  }
}
