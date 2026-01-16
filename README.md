# sandboxd

Minimal HTTP service that boots a Firecracker microVM, optionally injects files
into `/work`, runs a one-shot command, and returns `{ stdout, stderr, exit_code }`.

## Requirements

- Firecracker binary in `PATH`.
- Kernel image at `/home/milan/fc/hello-vmlinux.bin`.
- Rootfs image at `/home/milan/fc/rootfs.ext4`.
- Ability to mount loop devices (the service mounts the rootfs to inject files).

Update the constants in `main.go` if your paths differ.

## Running

```sh
go run main.go
```

The server listens on `:7777`.

## API

`POST /run`

Request body:

```json
{
  "cmd": "echo hello",
  "files": {
    "hello.sh": "#!/bin/sh\necho hello from file\n"
  },
  "timeout_ms": 2000
}
```

Behavior:

- `timeout_ms` defaults to 5000 when omitted or `<= 0`.
- If `files` is non-empty, the command runs from `/work`.
- The timeout is enforced on the host after Firecracker starts.
- If the guest does not reach init, the request fails with exit code 124.

Response body:

```json
{
  "stdout": "[guest] ...\n",
  "stderr": "",
  "exit_code": 0
}
```

## Notes

- The rootfs `init` is expected to log `[guest] init started` to the console.
- On timeout, the service kills the Firecracker process and returns exit code 124.
