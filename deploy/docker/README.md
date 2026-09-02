# Docker Compose example

This directory holds a single-instance docker compose example for
`excalidraw-wopi`.

## Steps

1. Edit `compose.yaml`. Set the three `EXCALIDRAW_WOPI_*` values and
   the Caddy site address.

2. Start the service.

   ```sh
   docker compose up -d
   ```

3. Register the editor in Drive. See the "Drive-side registration"
   section of [../../docs/DEPLOYMENT.md](../../docs/DEPLOYMENT.md).
