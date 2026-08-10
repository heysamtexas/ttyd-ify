# Vendored browser assets

These are third-party files, committed deliberately rather than fetched at build or run
time. `wtd` embeds them so the terminal page works on a tailnet with no route to the public
internet — a CDN `<script>` would fail exactly where this tool is most needed, and a build
step that downloads things would break "clone and `make install`".

Everything here is MIT licensed; see `LICENSE.xterm`, `LICENSE.addon-fit`,
`LICENSE.addon-webgl` and `LICENSE.addon-web-links`.

| File | Package | Version |
|---|---|---|
| `xterm.js` | [`@xterm/xterm`](https://www.npmjs.com/package/@xterm/xterm) | 6.0.0 |
| `xterm.css` | `@xterm/xterm` | 6.0.0 |
| `addon-fit.js` | [`@xterm/addon-fit`](https://www.npmjs.com/package/@xterm/addon-fit) | 0.11.0 |
| `addon-webgl.js` | [`@xterm/addon-webgl`](https://www.npmjs.com/package/@xterm/addon-webgl) | 0.19.0 |
| `addon-web-links.js` | [`@xterm/addon-web-links`](https://www.npmjs.com/package/@xterm/addon-web-links) | 0.12.0 |

The addons have no `peerDependencies` to pin them, so match them to `xterm` by release
rather than by range: 0.19.0 was published 51 seconds after 6.0.0, and 0.12.0 44 seconds
after. The addon that *looks* like the safer, more portable choice — `@xterm/addon-canvas` —
is not one. It is still at 0.7.0 from April 2024, peered to `@xterm/xterm@^5`, and was never
updated for 6. So the renderer ladder here has two rungs, WebGL and DOM, and no middle.

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
b51c9ca08dfd1b35e1f1b833334497a9a8412110569b26e03cb6e1ab7341fb61  addon-webgl-0.19.0.tgz
bf463cfff1af4bb1903509830c62ed06bcd0589039f15ea5fba71d7bae5a86da  addon-web-links-0.12.0.tgz
```

## Updating

```sh
V=6.0.0 F=0.11.0 W=0.19.0 L=0.12.0
curl -fsSL -o /tmp/xterm.tgz "https://registry.npmjs.org/@xterm/xterm/-/xterm-$V.tgz"
curl -fsSL -o /tmp/fit.tgz   "https://registry.npmjs.org/@xterm/addon-fit/-/addon-fit-$F.tgz"
curl -fsSL -o /tmp/webgl.tgz "https://registry.npmjs.org/@xterm/addon-webgl/-/addon-webgl-$W.tgz"
curl -fsSL -o /tmp/links.tgz "https://registry.npmjs.org/@xterm/addon-web-links/-/addon-web-links-$L.tgz"
sha256sum /tmp/xterm.tgz /tmp/fit.tgz /tmp/webgl.tgz /tmp/links.tgz   # record these in this file

# All four unpack to /tmp/package, so extract and copy one at a time or they overwrite:
#   xterm.tgz -> lib/xterm.js, css/xterm.css, LICENSE
#   fit.tgz   -> lib/addon-fit.js, LICENSE
#   webgl.tgz -> lib/addon-webgl.js, LICENSE
#   links.tgz -> lib/addon-web-links.js, LICENSE
tar xzf /tmp/xterm.tgz -C /tmp   # ...copy, then repeat for the addons. Then:
(cd cmd/wtd/web/vendor && sha256sum xterm.js xterm.css addon-fit.js addon-webgl.js addon-web-links.js > SHA256SUMS)
```

Then check the terminal by hand: attach to a session, run something full-screen (`htop`,
`vim`), resize the window, and confirm reflow — an xterm major bump is exactly the kind of
change that renders fine and breaks key handling.
