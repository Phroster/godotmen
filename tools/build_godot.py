"""Builds Godot 4.7.1 with the `ikemen` module and the engine patches (Windows, MSVC).

    python tools/build_godot.py --src D:\\godot            # editor, then the release template
    python tools/build_godot.py --src D:\\godot editor     # only the editor
    python tools/build_godot.py --src D:\\godot template   # only template_release
    python tools/build_godot.py --src D:\\godot --check    # only prepare the source tree, no build

--src is a clone of Godot 4.7.1-stable (or set the environment variable GODOT_SRC):

    git clone --depth 1 --branch 4.7.1-stable https://github.com/godotengine/godot.git

What it does:
1. Applies every godot/patches/*.patch in file-name order, skipping the ones already in the tree. A patch that is
   neither present nor applies cleanly stops the build.
2. Links <src>/modules/ikemen to godot/modules/ikemen with a directory junction. Not custom_modules=: that option
   adds include paths to every object and forces a full engine rebuild.
3. Runs SCons at below-normal priority. The outputs land in <src>/bin.
"""
import argparse
import os
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODULE_DIR = os.path.join(ROOT, "godot", "modules", "ikemen")
PATCH_DIR = os.path.join(ROOT, "godot", "patches")

COMMON = ["platform=windows", "accesskit=no", "d3d12=no"]
TARGETS = {
    "editor": ["target=editor"],
    "template": ["target=template_release", "production=yes"],
}


def log(msg):
    print("[build_godot] " + msg, flush=True)


def git(src, *args):
    return subprocess.run(["git", "-C", src] + list(args), capture_output=True, text=True)


def ensure_patches(src):
    patches = sorted(n for n in os.listdir(PATCH_DIR) if n.endswith(".patch"))
    if not patches:
        sys.exit("[build_godot] no patches in " + PATCH_DIR)
    for name in patches:
        patch = os.path.join(PATCH_DIR, name)
        if git(src, "apply", "--reverse", "--check", patch).returncode == 0:
            log("patch present: " + name)
            continue
        r = git(src, "apply", "--check", patch)
        if r.returncode != 0:
            sys.exit("[build_godot] %s is not in the tree and does not apply cleanly:\n%s" % (name, r.stderr))
        git(src, "apply", patch)
        log("patch applied: " + name)


def same_dir(a, b):
    try:
        return os.path.samefile(a, b)
    except OSError:
        return False


def ensure_junction(src):
    link = os.path.join(src, "modules", "ikemen")
    if os.path.lexists(link):
        if not same_dir(link, MODULE_DIR):
            sys.exit("[build_godot] %s exists but does not point at %s; remove it first." % (link, MODULE_DIR))
        log("modules/ikemen -> " + MODULE_DIR)
        return
    r = subprocess.run('cmd /c mklink /J "%s" "%s"' % (link, MODULE_DIR), capture_output=True, text=True)
    if r.returncode != 0 or not same_dir(link, MODULE_DIR):
        sys.exit("[build_godot] mklink /J failed: " + (r.stdout + r.stderr).strip())
    log("junction created: modules/ikemen -> " + MODULE_DIR)
    exclude = os.path.join(src, ".git", "info", "exclude")
    if os.path.isfile(exclude):
        with open(exclude, "a", encoding="utf-8") as f:
            f.write("\nmodules/ikemen\n")


def check_source(src):
    version = os.path.join(src, "version.py")
    if not os.path.isfile(version):
        sys.exit("[build_godot] no Godot source at %s (pass --src or set GODOT_SRC)" % src)
    with open(version, encoding="utf-8") as f:
        text = f.read()
    if "minor = 7" not in text or "patch = 1" not in text:
        log("warning: %s is not Godot 4.7.1; the patches may not apply" % src)


def build(src, target, jobs, log_dir):
    os.makedirs(log_dir, exist_ok=True)
    log_path = os.path.join(log_dir, "godot_%s.log" % target)
    scons = " ".join(['"%s"' % sys.executable, "-m", "SCons", "-C", '"%s"' % src] + COMMON + TARGETS[target] + ["-j%d" % jobs])
    cmd = 'cmd /c start "" /b /wait /belownormal ' + scons
    log("building %s -> %s" % (target, log_path))
    t0 = time.time()
    with open(log_path, "w", encoding="utf-8", errors="replace") as out:
        out.write(cmd + "\n")
        out.flush()
        code = subprocess.run(cmd, stdout=out, stderr=subprocess.STDOUT).returncode
    with open(log_path, encoding="utf-8", errors="replace") as f:
        tail = f.read().splitlines()[-6:]
    ok = code == 0 and any("done building targets" in line for line in tail)
    log("%s %s in %.0f s" % (target, "OK" if ok else "FAILED (exit %d)" % code, time.time() - t0))
    if not ok:
        print("\n".join(tail))
        sys.exit(1)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("what", nargs="?", default="both", choices=["editor", "template", "both"])
    ap.add_argument("--src", default=os.environ.get("GODOT_SRC"), help="Godot 4.7.1-stable source folder")
    ap.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 4) // 2), help="SCons jobs")
    ap.add_argument("--log-dir", default=os.path.join(ROOT, "bin", "logs"))
    ap.add_argument("--check", action="store_true", help="prepare and check the source tree only")
    args = ap.parse_args()
    if not args.src:
        sys.exit("[build_godot] pass --src <godot source> or set GODOT_SRC")

    check_source(args.src)
    ensure_patches(args.src)
    ensure_junction(args.src)
    if args.check:
        return 0
    for target in (["editor", "template"] if args.what == "both" else [args.what]):
        build(args.src, target, args.jobs, args.log_dir)
    log("done: the executables are in %s" % os.path.join(args.src, "bin"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
