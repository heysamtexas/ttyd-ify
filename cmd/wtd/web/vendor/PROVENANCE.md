# Vendored browser assets

These are third-party files, committed deliberately rather than fetched at build or run
time. `wtd` embeds them so the terminal page works on a tailnet with no route to the public
internet — a CDN `<script>` would fail exactly where this tool is most needed, and a build
step that downloads things would break "clone and `make install`".

Everything here is MIT licensed; see `LICENSE.xterm` and `LICENSE.addon-fit`.

| File | Package | Version |
|---|---|---|
| `xterm.js` | [`@xterm/xterm`](https://www.npmjs.com/package/@xterm/xterm) | 6.0.0 |
| `xterm.css` | `@xterm/xterm` | 6.0.0 |
| `addon-fit.js` | [`@xterm/addon-fit`](https://www.npmjs.com/package/@xterm/addon-fit) | 0.11.0 |

`xterm.js` ships pre-minified (a single-line UMD bundle), so nothing is minified here — the
files are byte-identical to what the published tarballs contain.

## Verifying what is here

```sh
sha256sum -c cmd/wtd/web/vendor/SHA256SUMS
```

Source tarball checksums, so a future update can be checked against the registry rather
than trusted:

```
908e66e04af6c8dc6b00dd3b54de088e2e81e5ed866284fd6c2fb3c2d1c7a3f6  xterm-6.0.0.tgz
26003b4517a132b64e4ff228fd88a5fda3fff5e606c76093f6dcff772e9ecec0  addon-fit-0.11.0.tgz
```

## Updating

```sh
V=6.0.0 F=0.11.0
curl -fsSL -o /tmp/xterm.tgz "https://registry.npmjs.org/@xterm/xterm/-/xterm-$V.tgz"
curl -fsSL -o /tmp/fit.tgz   "https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-$F.tgz"
sha256sum /tmp/xterm.tgz /tmp/fit.tgz          # record these in this file
tar xzf /tmp/xterm.tgz -C /tmp && tar xzf /tmp/fit.tgz -C /tmp   # both unpack to /tmp/package
# copy lib/xterm.js, css/xterm.css, lib/addon-fit.js and the LICENSE files here, then:
(cd cmd/wtd/web/vendor && sha256sum xterm.js xterm.css addon-fit.js > SHA256SUMS)
```

Then check the terminal by hand: attach to a session, run something full-screen (`htop`,
`vim`), resize the window, and confirm reflow — an xterm major bump is exactly the kind of
change that renders fine and breaks key handling.
