import { describe, expect, it, afterEach, vi } from 'vitest'
import { loadConfig } from './config'

function setConfigBlob(text: string) {
  const el = document.createElement('script')
  el.type = 'application/json'
  el.id = 'ew-config'
  el.textContent = text
  document.body.appendChild(el)
}

afterEach(() => {
  document.getElementById('ew-config')?.remove()
})

describe('loadConfig', () => {
  it('returns null for an empty blob', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    setConfigBlob('{}')
    expect(loadConfig()).toBeNull()
    spy.mockRestore()
  })

  it('returns null when the element is missing', () => {
    expect(loadConfig()).toBeNull()
  })

  it('returns a typed config for a filled blob', () => {
    const config = {
      fileId: 'file-1',
      fileName: 'board.excalidraw',
      userId: 'user-1',
      userName: 'Alice',
      canWrite: true,
      sessionToken: 'token-abc',
      apiBase: '/api',
      socketPath: '/socket.io',
      maxImageBytes: 10 * 1024 * 1024,
    }
    setConfigBlob(JSON.stringify(config))
    expect(loadConfig()).toEqual(config)
  })

  it('returns null and logs an error for malformed JSON', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    setConfigBlob('{not valid json')
    expect(loadConfig()).toBeNull()
    expect(spy).toHaveBeenCalled()
    spy.mockRestore()
  })

  it('returns null and logs an error when fileId is missing', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    setConfigBlob(JSON.stringify({ fileName: 'board.excalidraw' }))
    expect(loadConfig()).toBeNull()
    expect(spy).toHaveBeenCalled()
    spy.mockRestore()
  })

  it('returns null when fileId is empty', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    setConfigBlob(JSON.stringify({ fileId: '' }))
    expect(loadConfig()).toBeNull()
    spy.mockRestore()
  })
})
