#!/usr/bin/env python3
"""Fail the release build if a shipped Python module imports a local
module that is not shipped.

The checker builds the module map of the source tree (node/, core-py/
and proto/) and of the assembled bundle.  Every import found in a
shipped file -- static imports, relative imports and literal
__import__('...') calls -- that resolves to a source-tree module must
also resolve inside the bundle.  Imports that resolve in neither tree
are stdlib or third-party (requirements) imports and are ignored.
Entry modules that shipped binaries invoke by name are also required
to be present, so renaming or dropping an entry fails the build even
if nothing imports it statically.
"""
import argparse
import ast
import sys
from pathlib import Path

# Modules that shipped binaries / scripts invoke as `python -m <name>`
# or that the bundle documents as runnable entry points.
REQUIRED_ENTRIES = (
    'agent', 'agent.__main__', 'auditor', 'auditor.__main__',
    'pipeline_client', 'moe_gateway', 'tokenize_prompt',
    'privacy.server', 'fetch_t2i', 'content_policy',
)


def module_name(root, path):
    rel = path.relative_to(root)
    parts = list(rel.parts)
    if parts[-1] == '__init__.py':
        parts = parts[:-1]
    elif parts[-1].endswith('.py'):
        parts[-1] = parts[-1][:-3]
    else:
        return None
    return '.'.join(parts)


def module_map(roots):
    """Map dotted module name -> Path for every .py under roots."""
    result = {}
    for root in roots:
        root = Path(root)
        if not root.is_dir():
            continue
        for path in sorted(root.rglob('*.py')):
            name = module_name(root, path)
            if name:
                result.setdefault(name, path)
    return result


def resolve(name, source):
    """Return the longest dotted prefix of name present in source."""
    candidate = name
    while candidate:
        if candidate in source:
            return candidate
        if '.' not in candidate:
            return None
        candidate = candidate.rsplit('.', 1)[0]
    return None


def imported_names(path, package):
    """All absolute dotted names a file imports (best-effort static)."""
    try:
        tree = ast.parse(Path(path).read_text(errors='ignore'))
    except SyntaxError:
        return set()
    names = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                names.add(alias.name)
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                base_parts = package.split('.') if package else []
                keep = len(base_parts) - (node.level - 1)
                base = '.'.join(base_parts[:keep]) if keep > 0 else ''
                full = (base + '.' + node.module) if (base and node.module) \
                    else (base or node.module or '')
                if full:
                    names.add(full)
                    for alias in node.names:
                        names.add(full + '.' + alias.name)
                else:
                    for alias in node.names:
                        names.add(alias.name)
            elif node.module:
                names.add(node.module)
                for alias in node.names:
                    names.add(node.module + '.' + alias.name)
        elif isinstance(node, ast.Call) and isinstance(node.func, ast.Name) \
                and node.func.id == '__import__' and node.args \
                and isinstance(node.args[0], ast.Constant) \
                and isinstance(node.args[0].value, str):
            names.add(node.args[0].value)
    return names


def main(argv=None):
    parser = argparse.ArgumentParser(prog='check_import_closure')
    parser.add_argument('--source-root', action='append', required=True,
                        help='source tree root (repeatable)')
    parser.add_argument('--bundle-root', action='append', required=True,
                        help='assembled bundle root (repeatable)')
    args = parser.parse_args(argv)

    source = module_map(args.source_root)
    bundle = module_map(args.bundle_root)
    bundle_files = {}
    for root in args.bundle_root:
        root = Path(root)
        if not root.is_dir():
            continue
        for path in sorted(root.rglob('*.py')):
            name = module_name(root, path)
            if name:
                bundle_files.setdefault(name, (root, path))

    problems = []
    for entry in REQUIRED_ENTRIES:
        if entry not in bundle:
            problems.append(f'missing required entry module: {entry}')

    for name, (root, path) in sorted(bundle_files.items()):
        is_init = path.name == '__init__.py'
        package = name if is_init else (name.rsplit('.', 1)[0] if '.' in name else '')
        for imported in sorted(imported_names(path, package)):
            target = resolve(imported, source)
            if target is not None and target not in bundle:
                problems.append(
                    f'{path}: imports {imported!r} (source module '
                    f'{target!r}) which is not in the bundle')

    if problems:
        print('import closure check FAILED:', file=sys.stderr)
        for problem in problems:
            print(f'  - {problem}', file=sys.stderr)
        return 1
    print(f'import closure OK: {len(bundle)} modules shipped, '
          f'all local imports resolve inside the bundle')
    return 0


if __name__ == '__main__':
    sys.exit(main())
