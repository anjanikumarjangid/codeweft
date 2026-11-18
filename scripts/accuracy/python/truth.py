"""Prints the call edges jedi infers in a Python source tree, in codeweft's
naming: "caller callee". Python has no type checker that decides every call,
so this is a reference to compare with, not ground truth: disagreements are
worth reading both ways. Test files are left out, as codeweft keeps them
apart from the code.

Usage (jedi 0.19.2):
  python3 -m venv /tmp/jedi && /tmp/jedi/bin/pip install jedi==0.19.2
  /tmp/jedi/bin/python truth.py ROOT > truth.txt
"""
import ast, os, sys
import jedi

root = sys.argv[1]
project = jedi.Project(root, added_sys_path=[os.path.join(root, "src")])

def module_name(rel):
    m = rel[:-3]
    m = m[:-len("/__init__")] if m.endswith("/__init__") else m
    m = m[len("src/"):] if m.startswith("src/") else m
    return m.replace("/", ".")

edges = set()
for dirpath, dirs, files in os.walk(root):
    dirs[:] = [d for d in dirs if not d.startswith(".") and d not in ("tests", "docs", "build", "venv")]
    for f in files:
        if not f.endswith(".py"):
            continue
        path = os.path.join(dirpath, f)
        rel = os.path.relpath(path, root)
        if f.startswith("test_") or "/tests/" in "/" + rel:
            continue
        src = open(path).read()
        try:
            tree = ast.parse(src)
        except SyntaxError:
            continue
        script = jedi.Script(src, path=path, project=project)
        mod = module_name(rel)

        def visit(node, scope):
            for child in ast.iter_child_nodes(node):
                if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    visit(child, scope + [child.name])
                elif isinstance(child, ast.ClassDef):
                    visit(child, scope + [child.name])
                else:
                    if isinstance(child, ast.Call) and len(scope) > 0:
                        fn = child.func
                        if isinstance(fn, ast.Attribute):
                            line, col = fn.end_lineno, fn.end_col_offset - 1
                        elif isinstance(fn, ast.Name):
                            line, col = fn.lineno, fn.col_offset
                        else:
                            line = None
                        if line:
                            try:
                                defs = script.goto(line, col, follow_imports=True)
                            except Exception:
                                defs = []
                            for d in defs[:1]:
                                if d.full_name and d.type in ("function", "class"):
                                    callee = d.full_name
                                    if d.type == "class" and isinstance(fn, ast.Attribute) and fn.attr == "__init__":
                                        callee += ".__init__"
                                    edges.add(mod + "." + ".".join(scope) + " " + callee)
                    visit(child, scope)
        visit(tree, [])

for e in sorted(edges):
    print(e)
print(len(edges), "distinct call edges", file=sys.stderr)
