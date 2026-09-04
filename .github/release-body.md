`wtd` binaries for Linux. To deploy *these* bytes — on any box, with or without
a Go toolchain:

```sh
git clone https://github.com/heysamtexas/ttyd-ify && cd ttyd-ify
make fetch          # downloads the matching binary, verifies checksum + provenance
sudo ./install.sh   # NOT `make install` — see below
```

`make install` runs `make build` first whenever `go` is on the box, which
overwrites the binary `make fetch` just verified with a local rebuild. Both are
stamped from the same tag, so `wtd -version` and `/api/v1/meta` report the same
string either way and nothing downstream can tell them apart. Use `make install`
when you *want* a build from source; use `sudo ./install.sh` to install what you
just downloaded.

These binaries carry a signed build provenance attestation. To check one came
from this repo's release workflow rather than merely matching a checksum served
beside it:

```sh
gh attestation verify wtd-linux-amd64 --repo heysamtexas/ttyd-ify
```

`make install` enables `wt.service`, which serves `wtd` on `WT_PORT` (7681).
ttyd is retired as of #23: upgrading replaces it in place, and the install
refuses up front if your config sets `WT_AUTH` or `WT_TTYD_ARGS`, which `wtd`
cannot honor. Installing does not restart a running service — that command
drops connected clients, so it is left to you:

```sh
sudo systemctl restart wt.service
```

⚠ This serves a writable, unauthenticated shell. It is only as private as the
interface `WT_BIND` puts it on.
