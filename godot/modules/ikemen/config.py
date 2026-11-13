# Godotmen: Ikemen GO overlay module (engine singleton "Ikemen").
# Lives in godot/modules/ikemen of the Godotmen repository and is linked into the Godot tree as
# <godot source>/modules/ikemen through a directory junction (tools/build_godot.py), so the
# build does not need custom_modules= (that option prepends include paths to every object and would
# force a full engine rebuild).


def can_build(env, platform):
    # The overlay draws with OpenGL inside Godot's Compatibility renderer and loads a Windows DLL.
    return platform == "windows" and env["opengl3"]


def configure(env):
    pass
