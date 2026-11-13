# Godotmen

**Ikemen GO running inside Godot.** Godotmen loads Ikemen GO as a library into a Godot 4 game and lets it
simulate, draw and voice MUGEN / Ikemen characters with their own code, sprites, palettes, effects and
sounds, composited straight into your Godot scene. Nothing is converted or baked: character packs run as
they are.

You get Ikemen's fighting engine for the fighters and all of Godot for everything around them: 3D scenes,
cameras, menus, HUD, game modes, story, whatever your game needs.

This repository contains **no game content**: no characters, stages, sprites, sounds or music. You bring
your own MUGEN / Ikemen packs.

## Features

- **Ikemen GO's own engine.** Ikemen draws every fighter pixel with its own renderer (palettes, PalFX,
  blend modes, afterimages, explods, projectiles) and mixes every sound with its own mixer.
- **Tested against stock Ikemen GO.** The same fight played in stock Ikemen GO v1.0.0 and through Godotmen
  gives the same state for every character and helper and the same random seed on every tick
  ([parity test](docs/GUIDE.md#6-parity-test)).
- **In-process.** No second window, no second process, no sockets. Ikemen GO is built as `ikemen.dll` and
  loaded by a Godot engine module.
- **Composited into your scene.** Ikemen draws into Godot's own render target, after the 3D pass and before
  your HUD. Sprites stand in the 3D world as camera-facing cards and are depth-tested against your scene,
  so 3D props hide fighters behind them.
- **More than two fighters.** Fighters are spawned into slots (up to 128) from any number of packs, in
  teams, on a floor with depth.
- **Your game stays in charge.** GDScript sends inputs and commands and reads fighter state and events
  every tick: spawn, teleport, set life, force a move, pause, change palette or size, and more.

## Quick start

1. Download the latest zip from [Releases](https://github.com/Phroster/godotmen/releases) and unpack it. It
   has a Godot 4.7.1 editor and export template with Godotmen built in, `ikemen.dll`, and an example
   project.
2. Tell Ikemen which motif to use. Create `ikemen\save\config.ini`, pointing `Motif` at the `system.def`
   of your pack:

   ```ini
   [Common]
   States = data/functions.zss, data/system.zss
   Fx =
   Lua =
   [Config]
   Motif = D:/packs/mypack/data/system.def
   FirstRun = 0
   ```

3. Run the example with two fighters from your pack:

   ```
   Run-Example.cmd D:/packs/mypack/chars/kfm/kfm.def D:/packs/mypack/chars/kfm/kfm.def
   ```

   Player 1 is on the keyboard (A/D walk, W jump, S crouch, J K L and U I O attack). Player 2 is the AI.

For your own game, open your project with the included editor, set its renderer to Compatibility, copy the
`ikemen` folder into it and follow [the guide](docs/GUIDE.md#4-use-it-in-your-own-project).

## What it looks like in GDScript

```gdscript
var ik := Engine.get_singleton("Ikemen")

func _ready() -> void:
    var kfm := "D:/packs/mypack/chars/kfm/kfm.def"
    ik.start("", {
        "fighters": [{"def": kfm, "pal": 1, "ai": 0}, {"def": kfm, "pal": 2, "ai": 4}],
        "stage": "stages/overlay.def",
        "round_time": -1,
    })
    # Draw the fighters after the 3D pass and under canvas layer 1 (the HUD).
    ik.set_overlay(get_tree().root.get_viewport_rid(), 1, ik.DEPTH_TEST)

func _physics_process(_delta: float) -> void:
    if ik.status() == 1:
        ik.command({"op": "input", "h": 0, "bits": bits})
    ik.step()                        # one 60 Hz Ikemen tick
    var state := ik.get_state()      # position, life, state, animation... per fighter
    for event in ik.poll_events():   # hits, knockouts, screen shakes...
        pass
```

[example/main.gd](example/main.gd) is a complete, commented scene. The [guide](docs/GUIDE.md) has the full
reference: start options, every command, the state record, coordinates and events.

## How it works

```
Godot (60 Hz physics tick)   Ikemen.command(...) -> Ikemen.step() -> Ikemen.get_state() / poll_events()
Godot (every frame)          Ikemen.set_camera(view_projection, params)
Godot's renderer             3D pass -> [hook] -> canvas layers (HUD)
                                          |
        engine module `ikemen`:  GL state guard -> ikemen.dll draws into the viewport's framebuffer
```

| Part | Where | What |
| --- | --- | --- |
| Engine module `ikemen` | [godot/modules/ikemen](godot/modules/ikemen) | The `Ikemen` singleton: loads the DLL, ticks it, runs the render hook, and guards Godot's GL state around Ikemen's drawing. `AudioStreamIkemen` can route Ikemen's mix through Godot's audio buses. |
| Godot source patches | [godot/patches](godot/patches) | Three small patches to Godot 4.7.1: a viewport overlay hook, a scene hook in the Compatibility renderer, and a `DirAccess` fix so Godot and Ikemen do not fight over the process working directory. |
| `ikemen.dll` | [ikemen](ikemen) | Ikemen GO v1.0.0 with a host layer: boot without a window, step one tick on request, draw into the caller's GL context, host-fed inputs, state and event readback. |

A foreign renderer can only draw in the middle of Godot's frame through engine hooks, so Godotmen is a
**custom Godot build**, not a GDExtension or an addon. A stock Godot cannot run it. The releases include
the built editor and export template, so you only compile Godot if you want to change the engine.

## Requirements

- Windows 10 or 11, 64-bit.
- A GPU with OpenGL 3.3. Your project must use Godot's **Compatibility** renderer; Forward+ and Mobile are
  not supported.
- A MUGEN or Ikemen GO character pack.

## Building from source

You need Godot 4.7.1 source with Visual Studio 2022 for the engine, and MSYS2 with the clang64 Go toolchain
for the DLL.

```
python tools/build_godot.py --src path\to\godot
ikemen\build\build_dll.cmd
python tools/package.py --src path\to\godot --version 0.1.0
```

Details are in [the guide](docs/GUIDE.md).

## Repository layout

| Path | Contents |
| --- | --- |
| `godot/modules/ikemen` | The Godot engine module |
| `godot/patches` | The three Godot 4.7.1 source patches |
| `ikemen/` | The Ikemen GO fork and Ikemen's game folder (`data/`, `font/`, `external/`, an empty stage) |
| `ikemen/src/host_*.go` | The host layer |
| `example/` | The example project |
| `docs/GUIDE.md` | The guide and API reference |
| `tools/build_godot.py` | Patches a Godot source tree, links the module and builds the editor and template |
| `tools/package.py` | Makes the release zip |
| `tools/ovhost` | A native test host: runs a fight through the DLL without Godot and writes PNG frames |
| `tools/parity` | The parity test against stock Ikemen GO v1.0.0 |

## Status and limits

Godotmen is a preview. It works, and these are its current edges:

- Windows only, Compatibility renderer only.
- One Ikemen instance per process.
- Parity with stock Ikemen GO is tested on simulation state for the stock two-fighter match. Rendered
  pixels and mixed sound are not compared. The slot mode with more than two fighters changes Ikemen's rules
  on purpose, so it has no stock reference.
- Sound through Godot's audio buses (`audio: "host"`) and the `pipeline` draw mode are included but have
  not been tested in this version. The defaults, Ikemen's own sound output and direct drawing, are what
  the example uses.
- Ikemen's own menus are not shown, its lifebar is off by default, and the included stage is empty: your
  Godot project draws the scene and the HUD.

## Credits and licence

- Godotmen's own code is under the [MIT licence](LICENSE).
- [Ikemen GO](https://github.com/ikemen-engine/Ikemen-GO) by the Ikemen GO team, MIT licence
  ([ikemen/LICENCE.txt](ikemen/LICENCE.txt)). The `ikemen` folder is a modified copy of Ikemen GO v1.0.0.
- [Godot Engine](https://godotengine.org), MIT licence.
- Godotmen is not affiliated with or endorsed by the Godot project or the Ikemen GO team.
- M.U.G.E.N is by Elecbyte. Character and stage packs belong to their authors and are not part of this
  repository.
