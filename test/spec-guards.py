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
"""
import json, re, sys, glob, os
import yaml

FAIL = []


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


def check_served_docs():
    """Every /docs/<file> the spec mentions must be in handleDocs' allowlist and on disk."""
    allowed = set(re.findall(r'"([\w.-]+\.md)":\s*"text/markdown', open('cmd/wtd/docs.go').read()))
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


check_pointers()
check_payload()
check_served_docs()
if FAIL:
    print('spec-guards: FAIL')
    for f in FAIL:
        print(f'  {f}')
    sys.exit(1)
print('spec-guards: pointers resolve, served descriptions carry no citations')
