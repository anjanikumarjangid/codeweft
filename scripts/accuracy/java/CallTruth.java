import com.sun.source.tree.*;
import com.sun.source.util.*;
import javax.lang.model.element.*;
import javax.tools.*;
import java.nio.file.*;
import java.util.*;
import java.util.stream.*;

/**
 * Compiles a Java source tree with javac (as far as its dependencies allow:
 * missing annotation libraries do not stop method calls from resolving) and
 * prints every resolved call as "caller callee", in codewiki's naming:
 * package, classes, then the method, or the class name again for a
 * constructor. Calls inside anonymous classes are left out, as codewiki
 * does; creating an anonymous subclass of a class calls that class's
 * constructor.
 *
 * Usage: javac -d /tmp/ct CallTruth.java && java -cp /tmp/ct CallTruth SRC_DIR > truth.txt
 */
public class CallTruth {
    static String qual(Element e) {
        Deque<String> parts = new ArrayDeque<>();
        Element x = e;
        while (x != null && !(x instanceof PackageElement)) {
            if (x instanceof TypeElement || x instanceof ExecutableElement) {
                String n = x.getSimpleName().toString();
                if (x.getKind() == ElementKind.CONSTRUCTOR) n = x.getEnclosingElement().getSimpleName().toString();
                if (n.isEmpty()) return null; // anonymous class
                parts.addFirst(n);
            }
            x = x.getEnclosingElement();
        }
        if (x == null) return null;
        String pkg = ((PackageElement) x).getQualifiedName().toString();
        return (pkg.isEmpty() ? "" : pkg + ".") + String.join(".", parts);
    }

    public static void main(String[] args) throws Exception {
        List<Path> files;
        try (Stream<Path> s = Files.walk(Path.of(args[0]))) {
            files = s.filter(p -> p.toString().endsWith(".java")).collect(Collectors.toList());
        }
        JavaCompiler jc = ToolProvider.getSystemJavaCompiler();
        StandardJavaFileManager fm = jc.getStandardFileManager(null, null, null);
        JavacTask task = (JavacTask) jc.getTask(null, fm, d -> {}, List.of("-proc:none", "-Xmaxerrs", "100000"), null,
                fm.getJavaFileObjectsFromPaths(files));
        Iterable<? extends CompilationUnitTree> units = task.parse();
        task.analyze();
        Trees trees = Trees.instance(task);
        Set<String> edges = new TreeSet<>();
        Set<String> anon = new TreeSet<>();
        for (CompilationUnitTree cu : units) {
            new TreePathScanner<Void, Void>() {
                String caller() {
                    for (TreePath p = getCurrentPath(); p != null; p = p.getParentPath()) {
                        Tree t = p.getLeaf();
                        if (t instanceof ClassTree c && c.getSimpleName().toString().isEmpty()) return "ANON";
                        if (t instanceof MethodTree) {
                            Element e = trees.getElement(p);
                            return e == null ? null : qual(e);
                        }
                    }
                    return null;
                }
                void record(Element callee) {
                    if (!(callee instanceof ExecutableElement)) return;
                    String c = caller(), t = qual(callee);
                    if (c == null || t == null) return;
                    if (c.equals("ANON")) { anon.add(t); return; }
                    edges.add(c + " " + t);
                }
                @Override public Void visitMethodInvocation(MethodInvocationTree n, Void v) {
                    record(trees.getElement(new TreePath(getCurrentPath(), n.getMethodSelect())));
                    return super.visitMethodInvocation(n, v);
                }
                @Override public Void visitNewClass(NewClassTree n, Void v) {
                    Element ctor = trees.getElement(getCurrentPath());
                    if (n.getClassBody() != null && ctor != null) {
                        // new Base() { ... }: the anonymous class's constructor
                        // runs Base's; new Iface() { ... } calls nothing of Iface.
                        TypeElement anon = (TypeElement) ctor.getEnclosingElement();
                        Element sup = anon.getSuperclass() instanceof javax.lang.model.type.DeclaredType d ? d.asElement() : null;
                        if (sup != null && !sup.toString().equals("java.lang.Object")) {
                            for (Element m : sup.getEnclosedElements()) {
                                if (m.getKind() == ElementKind.CONSTRUCTOR) { record(m); break; }
                            }
                        }
                    } else {
                        record(ctor);
                    }
                    return super.visitNewClass(n, v);
                }
            }.scan(cu, null);
        }
        edges.forEach(System.out::println);
        System.err.println(files.size() + " files, " + edges.size() + " distinct call edges; calls inside anonymous classes skipped: " + anon.size() + " distinct callees");
    }
}
