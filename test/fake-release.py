#!/usr/bin/env python3
"""Serve a directory as if it were github.com's release endpoints, so fetch.sh can be tested offline.

    fake-release.py <port> <root-dir> <latest-tag>

fetch.sh's refusals are the point of testing it: a corrupt download, an asset missing from
SHA256SUMS, a failed provenance check. None of those can be driven against the real GitHub without
publishing a broken release, so `WT_FETCH_ORIGIN` points here instead and the fixtures are whatever
the test wants them to be.

Two endpoints, matching the shapes fetch.sh depends on:

    /<owner>/<repo>/releases/latest            -> 302 to /<owner>/<repo>/releases/tag/<latest-tag>
    /<owner>/<repo>/releases/download/<tag>/<asset>  -> <root>/<tag>/<asset>

The redirect is not decoration: fetch.sh reads the tag out of curl's effective URL, so a server that
answered 200 here would leave that parsing untested.
"""

import functools
import http.server
import os
import posixpath
import sys


class FakeRelease(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, latest_tag: str, **kwargs):
        self.latest_tag = latest_tag
        super().__init__(*args, **kwargs)

    def do_GET(self):  # noqa: N802 - the stdlib spells it this way
        parts = self.path.strip("/").split("/")
        # /<owner>/<repo>/releases/latest
        if len(parts) == 4 and parts[2:4] == ["releases", "latest"]:
            target = f"/{parts[0]}/{parts[1]}/releases/tag/{self.latest_tag}"
            self.send_response(302)
            self.send_header("Location", target)
            self.end_headers()
            return
        # The redirect target itself. Nothing reads its body — fetch.sh only wants the URL — so an
        # empty 200 is the whole contract.
        if len(parts) == 5 and parts[2:4] == ["releases", "tag"]:
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        super().do_GET()

    do_HEAD = do_GET  # curl -I takes this path, which is how fetch.sh resolves the latest tag

    def translate_path(self, path):
        """Map /<owner>/<repo>/releases/download/<tag>/<asset> onto <root>/<tag>/<asset>."""
        parts = path.strip("/").split("/")
        if len(parts) == 6 and parts[2:4] == ["releases", "download"]:
            return os.path.join(self.directory, parts[4], parts[5])
        # Anything else stays inside the root, and never above it.
        return os.path.join(self.directory, posixpath.normpath(path).lstrip("/"))

    def log_message(self, *args):
        pass  # the test's output is the interesting one


def main():
    if len(sys.argv) != 4:
        raise SystemExit(f"usage: {sys.argv[0]} <port> <root-dir> <latest-tag>")
    port, root, latest = int(sys.argv[1]), sys.argv[2], sys.argv[3]
    handler = functools.partial(FakeRelease, directory=root, latest_tag=latest)
    with http.server.ThreadingHTTPServer(("127.0.0.1", port), handler) as httpd:
        httpd.serve_forever()


if __name__ == "__main__":
    main()
