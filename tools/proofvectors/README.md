# proofvectors

This tool creates the golden vectors that `internal/proof/proof_test.go`
checks against. It signs each vector with a fixed test RSA key, and it
checks each signature against Drive's own `verify_wopi_proof`, so a pass
in `go test` means Drive itself accepts the signature, not only that our
Go code agrees with our own Python port of the math.

## Regenerate the vectors

1. Get a clone of `suitenumerique/drive`, pinned to the version this
   tool targets (v0.21.1 at the time of writing).
2. Set up a Python 3.12+ virtual environment with `cryptography`:

   ```sh
   python3 -m venv .venv
   .venv/bin/pip install cryptography
   ```

3. Run the generator with the path to the Drive clone as the only
   argument:

   ```sh
   .venv/bin/python tools/proofvectors/generate.py /path/to/drive
   ```

This writes `internal/proof/testdata/vectors.json`. It reuses the test
key at `internal/proof/testdata/test_key.pem` if that file exists, and
it creates one on first run. Commit both files.

If the host Python cannot install `cryptography` (for example, no
compiler toolchain), run the generator in a container instead:

```sh
docker run --rm --network=host -v "$PWD":/work -w /work \
  python:3.12-slim bash -c \
  "pip install --quiet cryptography && \
   python tools/proofvectors/generate.py /path/to/drive"
```

## Why re-run against Drive's own code

Drive's proof math lives in `src/backend/wopi/utils/signature.py`. It
has no Django import, so this tool loads it directly by file path with
`importlib.util.spec_from_file_location`, without a Django settings
module. The generator builds `expected_proof` and signs it, then calls
Drive's `verify_wopi_proof` with the signature it just produced. A
vector only lands in `vectors.json` when that check passes, and each
vector's `verified_by_drive` field records the check.

## Test key

`internal/proof/testdata/test_key.pem` is a fixed, throwaway RSA 2048
key. Its file header says so, and no other part of the codebase reads
this file. Do not use it outside `internal/proof`'s tests.
