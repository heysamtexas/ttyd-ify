#!/usr/bin/env python3
"""Guards for the editorial rule in api/openapi.yaml. See its header comment.

Two independent checks, because the trim that removed ~9KB of internals from the served
spec created one new failure mode and relied on one convention:

  pointers  Every "<doc>.md §N" reference must resolve to a real section heading. The spec
            now delegates mechanism to those sections, so a renumber that silently breaks a
            pointer is the cost of delegating.
  payload   No *loaded* description may carry a source citation or a verification tag. It
            runs on the parsed YAML, so `#` comments are exempt by construction — which is
            exactly the rule: provenance in comments, never in what a client downloads.
  served    Every /docs/ path the spec mentions must be a document this binary actually
            serves. The spec now cites its companions by URL rather than by filename, which
            only helps if the URL resolves; a dead one is worse than the filename was.
  labelled  Every served document says up front that it is the maintainer's copy. The payload
            rule above does not reach these files, so labelling is what stands in for it; see
            cmd/wtd/docs.go for why they are labelled rather than filtered.
  doclinks  Every markdown link in a served document must resolve for a reader who has only
            this server. Same failure as `served`, one layer down: `](openapi.yaml)` reads
            fine in a checkout and 404s under /docs/.
"""
import json, re, sys, glob, os
import yaml

FAIL = []

# The greppable half of the maintainer's-copy notice. Every served document must open with it.
MARKER = "**Maintainer's copy, served for reference.**"


def check_pointers():
    ref = re.compile(r'(session-lifecycle|ws-protocol|compatibility)\.md\s*§\s*(\d+[a-z]?)')
    heads = {}
    for doc in ('session-lifecycle', 'ws-protocol', 'compatibility'):
        path = f'api/{doc}.md'
        if not os.path.exists(path):
            continue
        heads[doc] = {m.group(1) for m in re.finditer(r'^##\s+(\d+[a-z]?)\.', open(path).read(), re.M)}
    for src in glob.glob('api/*.md') + glob.glob('api/*.yaml') + glob.glob('.claude/rules/*.md'):
        for lineno, line in enumerate(open(src), 1):
            for doc, sec in ref.findall(line):
                if doc not in heads:
                    FAIL.append(f'{src}:{lineno}: points at api/{doc}.md, which does not exist')
                elif sec not in heads[doc]:
                    FAIL.append(f'{src}:{lineno}: points at {doc}.md §{sec}; that section does not exist '
                                f'(has {sorted(heads[doc], key=str)})')


def check_payload():
    banned = re.compile(r'bin/wt:\d|\.swift:\d|\.go:\d|\[LAB\]|\[LIVE\]')
    spec = yaml.safe_load(open('api/openapi.yaml'))

    def walk(node, path='$'):
        if isinstance(node, dict):
            for k, v in node.items():
                if k in ('description', 'summary') and isinstance(v, str):
                    if m := banned.search(v):
                        FAIL.append(f'api/openapi.yaml: {path}.{k} contains {m.group(0)!r} — served '
                                    f'descriptions carry no citations; move it to api/*.md or a # comment')
                else:
                    walk(v, f'{path}.{k}')
        elif isinstance(node, list):
            for i, v in enumerate(node):
                walk(v, f'{path}[{i}]')

    walk(spec)


def served_docs():
    """handleDocs' allowlist — the set of api/*.md a client can actually fetch."""
    return set(re.findall(r'"([\w.-]+\.md)":\s*"text/markdown', open('cmd/wtd/docs.go').read()))


def check_served_docs():
    """Every /docs/<file> the spec mentions must be in handleDocs' allowlist and on disk."""
    allowed = served_docs()
    if not allowed:
        FAIL.append('cmd/wtd/docs.go: could not read the docAssets allowlist; this check would '
                    'pass vacuously')
        return
    for lineno, line in enumerate(open('api/openapi.yaml'), 1):
        for name in re.findall(r'/docs/([\w.-]+\.md)', line):
            if name not in allowed:
                FAIL.append(f'api/openapi.yaml:{lineno}: cites /docs/{name}, which handleDocs '
                            f'does not serve (serves {sorted(allowed)})')
            elif not os.path.exists(f'cmd/wtd/docs/{name}'):
                FAIL.append(f'cmd/wtd/docs/{name} is missing — run: make spec')


def check_served_labelled():
    """Every served document carries the maintainer's-copy notice, near enough the top to be
    the first thing read. The wording is free; the marker is not, because a notice nobody can
    grep for is a notice the next document silently ships without."""
    for name in sorted(served_docs()):
        path = f'api/{name}'
        if not os.path.exists(path):
            FAIL.append(f'cmd/wtd/docs.go serves {name}, but api/{name} does not exist — the '
                        f'allowlist and the source of truth disagree')
            continue
        head = ''.join(open(path).readlines()[:12])
        if MARKER not in head:
            FAIL.append(f'{path}: served at /docs/{name} but does not carry {MARKER!r} in its '
                        f'first 12 lines — every served document must say it is the maintainer\'s '
                        f'copy and name /openapi.json as the contract')


def check_doc_links():
    """Every markdown link in a served document resolves for a reader who has only this server.

    Exempt: absolute URLs and bare #anchors. Everything else must be a served document (by
    bare name, since /docs/<x> makes a relative link resolve) or /openapi.json. A link to
    api/openapi.yaml is the specific mistake this catches — correct in a checkout, 404 served.
    """
    allowed = served_docs()
    if not allowed:
        return  # check_served_docs reports the vacuous case
    reachable = allowed | {f'/docs/{n}' for n in allowed} | {'/openapi.json'}
    for name in sorted(allowed):
        path = f'api/{name}'
        if not os.path.exists(path):
            continue  # check_served_labelled reports the missing source
        fenced = False
        for lineno, line in enumerate(open(path), 1):
            # Fenced blocks hold sample code, where ](...) is Go or shell rather than a link.
            if line.lstrip().startswith('```'):
                fenced = not fenced
                continue
            if fenced:
                continue
            for target in re.findall(r'\]\(([^)\s]+)\)', line):
                if target.startswith(('http://', 'https://', '#')):
                    continue
                if target.split('#', 1)[0] not in reachable:
                    FAIL.append(f'{path}:{lineno}: links to {target!r}, which this binary does '
                                f'not serve — a reader of /docs/{name} cannot follow it '
                                f'(serves {sorted(reachable)})')


check_pointers()
check_payload()
check_served_docs()
check_served_labelled()
check_doc_links()
if FAIL:
    print('spec-guards: FAIL')
    for f in FAIL:
        print(f'  {f}')
    sys.exit(1)
print('spec-guards: pointers resolve, served descriptions carry no citations, '
      'served documents are labelled and their links resolve')
