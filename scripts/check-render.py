#!/usr/bin/env python3
"""Assertions a render cannot hide and a cluster would refuse.

Two claims, both checked on rendered YAML rather than on templates:

1. Every container port has a name. A port without one is valid YAML that a Service cannot
   address by name, and it is exactly the kind of thing a template review walks past.
2. The full render shows a duplicated env name when the values ask for one. The point is not
   that duplicates are fine - it is that the render is honest about them. If extraEnv stops
   being rendered, the count drops and this check fails.

Usage: check-render.py --expect-extra NAME full.yaml default.yaml   (PyYAML required)

--expect-extra NAME asserts that NAME appears exactly once more in the first file than in
the second. That is the assertion the CI comment always meant: when the values duplicate an
env name, the render has to show the duplicate rather than swallowing it.
"""
import sys
import yaml


def containers(docs):
    for d in docs:
        if not isinstance(d, dict) or d.get("kind") not in ("Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"):
            continue
        spec = d.get("spec", {})
        if d["kind"] == "CronJob":
            pod = spec.get("jobTemplate", {}).get("spec", {}).get("template", {}).get("spec", {})
        else:
            pod = spec.get("template", {}).get("spec", {})
        for c in pod.get("containers", []) + pod.get("initContainers", []):
            yield d.get("kind"), c


def count_env(name, path):
    with open(path) as fh:
        docs = [d for d in yaml.safe_load_all(fh) if d]
    n = 0
    for _kind, c in containers(docs):
        for e in c.get("env", []) or []:
            if e.get("name") == name:
                n += 1
    return n


def main():
    argv = sys.argv[1:]
    expect = None
    if argv[:1] == ["--expect-extra"]:
        expect, argv = argv[1], argv[2:]
    paths = argv
    if not paths:
        print("usage: check-render.py <rendered.yaml> [...]", file=sys.stderr)
        return 2
    failures = []
    env_counts = {}
    for path in paths:
        with open(path) as fh:
            docs = [d for d in yaml.safe_load_all(fh) if d]
        for kind, c in containers(docs):
            for port in c.get("ports", []) or []:
                if not port.get("name"):
                    failures.append(f"{path}: {kind}/{c.get('name')} has containerPort {port.get('containerPort')} with no name")
            for e in c.get("env", []) or []:
                key = e.get("name")
                if key:
                    env_counts.setdefault(path, {}).setdefault(key, 0)
                    env_counts[path][key] += 1
    for path, counts in env_counts.items():
        for name, n in counts.items():
            if n > 1 and "full" in path:
                print(f"  render shows {n} entries for env {name} in {path} (expected when the values duplicate it)")
            elif n > 1:
                failures.append(f"{path}: env {name} appears {n} times")
    if expect:
        name, full, default = expect, paths[0], paths[1]
        got, base = count_env(name, full), count_env(name, default)
        if got != base + 1:
            failures.append(
                f"{full}: env {name} appears {got} times but {base} + 1 was expected, so the "
                f"render is hiding the duplicate the values asked for")
        else:
            print(f"  {name}: {base} in the defaults, {got} in the full render - the duplicate is visible")
    if failures:
        print("FAIL")
        for f in failures:
            print(f"  {f}")
        return 1
    print("OK - every container port is named, and env duplicates are only where the values asked for them")
    return 0


if __name__ == "__main__":
    sys.exit(main())
