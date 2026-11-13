# Godotmen guide

Everything here was run on Windows 11 with Godot 4.7.1 and three MUGEN packs.

## 1. Build Godot with the module

1. Get the Godot 4.7.1-stable source:

   ```
   git clone --depth 1 --branch 4.7.1-stable https://github.com/godotengine/godot.git
   ```

2. Patch the source, link the module and build:

   ```
   python tools/build_godot.py --src path\to\godot editor
   ```

   The script applies `godot/patches/*.patch`, links `godot/modules/ikemen` into the source tree as
   `modules/ikemen` with a directory junction, and builds into `godot\bin`. Leave out `editor` to build the
   release template as well. It needs Visual Studio 2022 (C++), Python and SCons (`pip install scons`).

   A cold editor build took about 30 minutes with 6 jobs. After that, a change to the module alone rebuilds
   in under a minute.

## 2. Build `ikemen.dll`

1. Install [MSYS2](https://www.msys2.org/) at `C:\msys64`, then in its shell:

   ```
   pacman -S mingw-w64-clang-x86_64-{go,clang,pkgconf,SDL2,libxmp,zlib}
   ```

2. Build the library (about 2 minutes the first time):

   ```
   ikemen\build\build_dll.cmd
   ```

   This makes `ikemen/bin/ikemen.dll` and copies `SDL2.dll` and `libxmp.dll` beside it.

3. Tell Ikemen which motif to use. Create `ikemen/save/config.ini` and point `Motif` at the `system.def`
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

### Pack layout

Fighters are loaded by the path of their `.def` file. For a fighter at `<pack>/chars/<name>/<name>.def`,
Ikemen takes that pack's common files (`fightfx.air`, `fightfx.sff`, `common.snd`, `fight.def`) from
`<pack>/data`, or from the first folder under `<pack>/data` that has them. Fighters from several packs can
be in the same fight, each with its own pack's effects.

## 3. Run the example

[example](../example) is a small Godot project: a floor, two pillars, a camera and a HUD, with two fighters
from your pack. Player 1 is on the keyboard (A/D walk, W jump, S crouch, J K L and U I O attack), player 2
is the AI.

```
path\to\godot\bin\godot.windows.editor.x86_64.exe --path example -- --p1=D:/packs/mypack/chars/kfm/kfm.def --p2=D:/packs/mypack/chars/kfm/kfm.def
```

[example/main.gd](../example/main.gd) is about 170 lines and shows the whole loop: start, overlay, input,
step, state, events, camera.

## 4. Use it in your own project

Set the project's renderer to **Compatibility**. Copy the `ikemen` folder (with `bin/`) into the project,
so the library is at `res://ikemen/bin/ikemen.dll`, or set the environment variable `IKEMEN_DLL` to its
absolute path. The module registers an engine singleton named `Ikemen`.

```gdscript
var ik := Engine.get_singleton("Ikemen")

func _ready() -> void:
    # Ikemen boots a match with two fighters; more are spawned with the "spawn" command.
    var kfm := "D:/packs/mypack/chars/kfm/kfm.def"
    var result: int = ik.start("", {       # "" is res://ikemen
        "fighters": [{"def": kfm, "pal": 1, "ai": 0}, {"def": kfm, "pal": 2, "ai": 4}],
        "stage": "stages/overlay.def",
        "round_time": -1,
    })
    if result < 0:
        push_error(ik.last_error())
        return
    # Draw Ikemen after the 3D pass and under canvas layers 1 and up (your HUD).
    ik.set_overlay(get_tree().root.get_viewport_rid(), 1, ik.DEPTH_TEST)

func _physics_process(_delta: float) -> void:
    if ik.status() == 1:  # 0 booting, 1 ready, -1 failed
        ik.command({"op": "input", "h": 0, "bits": bits})
    ik.step()                                        # one 60 Hz Ikemen tick
    var state: PackedFloat32Array = ik.get_state()   # 32 floats per fighter
    for event in ik.poll_events():                   # one Dictionary each
        pass

func _process(_delta: float) -> void:
    var cam := get_viewport().get_camera_3d()
    var view_proj := cam.get_camera_projection() * Projection(cam.get_camera_transform().affine_inverse())
    var right := cam.global_transform.basis.x
    ik.set_camera(view_proj, PackedFloat32Array([0.015, 0.0, 0.0, right.x, right.z, 0.0]))
```

### The `Ikemen` singleton

| Call | Does |
| --- | --- |
| `start(ikemen_root, config)` | Loads the DLL and boots Ikemen without a window. `""` is `res://ikemen` |
| `status()` | 0 booting, 1 ready, -1 failed |
| `command(dict)` | Sends one host command; needs an `"op"` key. Returns 0 or more, or a negative error |
| `step()` | Runs one 60 Hz tick |
| `get_state()` | Fighter records, 32 floats each |
| `poll_events()` | Events since the last call |
| `set_overlay(viewport_rid, first_hud_layer, flags)` | Installs the render hook on a viewport. Flags: `DEPTH_TEST`, `DEBUG_CLSN` |
| `clear_overlay()` | Removes the render hook |
| `set_camera(view_proj, params)` | The camera Ikemen draws with |
| `get_render_stats()` | Frame counts, timings and GL errors, for debugging |
| `last_error()` | The most recent error message |
| `shutdown()` | Removes the hook and stops Ikemen |

Every call is safe without the DLL: it returns an error code and `last_error()` says why.

### Coordinates

Host positions are in **host units**: one unit is one pixel of a fighter whose localcoord width is
`ik_localcoord` (320 by default).

- `x` runs along the stage, 0 at the stage centre.
- `depth` runs into the stage, 0 at the stage's z centre.
- `height` is up from the ground.

`set_camera`'s `params` place those units in your 3D world:

| Index | Meaning |
| --- | --- |
| 0 | World units per host unit (0 means 0.01) |
| 1, 2 | World x and z of host x 0 and depth 0 |
| 3, 4 | The direction sprite cards face along, in the x/z plane: the camera's right vector (0, 0 means +x) |
| 5 | World y of the floor |
| 6 | 1 for a standard depth buffer; leave 0 in Godot (reverse-Z) |
| 7 | Depth bias toward the camera |

Without the `DEPTH_TEST` flag Ikemen draws flat 2D over the frame and ignores the camera.

### Start config

| Key | Default | Meaning |
| --- | --- | --- |
| `fighters` | required | The two boot fighters: `[{def, pal, ai}, {def, pal, ai}]` |
| `stage` | required | A stage `.def`. `stages/overlay.def` is an empty stage with wide bounds |
| `round_time` | | -1 for no round timer |
| `nosound`, `nomusic` | false, true | Sound off; Ikemen's own music off |
| `audio` | `"sdl"` | `"host"` sends Ikemen's mix through `AudioStreamIkemen` and Godot's audio buses |
| `ffa` | true | Fighter slots with teams. false is a stock two-side match |
| `slots` | 128 | Fighter slots, 2 to 128 |
| `boot_fighters` | `"keep"` | `"disable"` hides the two boot fighters |
| `intro` | false | Boot fighters play their round intro |
| `lifebar` | false | Ikemen draws its own lifebar |
| `ik_localcoord` | 320 | The localcoord width whose pixel is one host unit |
| `origin_x`, `origin_depth` | 0 | Host x and depth of the stage centre |
| `depth_scale` | 1 | Ikemen z per host depth unit |
| `smooth` | true | Texel smoothing on 3D cards, so distant sprites do not shimmer |
| `screen_sprites` | true | Draw screen-space sprites flat over the frame |
| `floor_rule` | true | Keep sprite pixels below the ground line from being cut by your floor |
| `huge_cut` | false | Drop sprites as large as the screen |
| `corpse_limit` | 24 | Dead fighters kept on the floor |
| `load_workers` | 2 | Fighters loading at once, 1 to 4 |
| `pipeline` | false | Draw from snapshots while the next tick simulates |
| `parity_mode` | false | A stock match in flat 2D, for the parity test |

### Host commands

`h` is a fighter handle: 0 and 1 for the boot fighters, then what `spawn` returns.

| Op | Arguments | Does |
| --- | --- | --- |
| `spawn` | `def`, `team`, `pal`, `x`, `depth`, `facing`, `ai`, `life`, `attack`, `defense`, `size` | Loads a fighter into a free slot and returns its handle. Only `def` is required |
| `despawn` | `h` | Removes a fighter and everything it owns |
| `prepare` | `def` | Compiles a fighter ahead of its spawn; returns a handle or -1 |
| `prepared` | `h`, `def` | 1 ready, 0 loading, -1 gone |
| `clear_prepared` | | Drops prepared fighters nobody spawned |
| `packs_ready` | | 1 when the packs' effects have loaded |
| `input` | `h`, `bits`, `zmove` | Held buttons and depth walk (-1 to 1), kept until changed |
| `force_command` | `h`, `name`, `ticks` | Makes a named command of the fighter's `.cmd` read as active |
| `focus` | `h`, `target` | The fighter's opponent; -1 clears |
| `face` | `h`, `free`, `dir` | Host-steered facing, `dir` -1, 0 or 1 |
| `team` | `h`, `team` | Team number |
| `ai` | `h`, `level` | AI level 0 to 8 |
| `set_pos` | `h`, `x`, `depth`, `height`, `facing` | Teleport; velocity is zeroed |
| `nudge` | `h`, `x`, `depth` | Move along the floor, keeping height, velocity and state |
| `var` | `h`, `i` or `f`, `value` | Sets an int or float variable of the fighter |
| `life` | `h`, `value`, `max`, `by` | Sets life, max life, or credits damage to `by` |
| `attack`, `defense`, `motion` | `h`, `mul` | Multipliers |
| `control` | `h`, `kind`, `ticks`, `by` | `freeze`, `stun`, `launch`, `knockdown` or `knock` |
| `pose` | `h`, `kind` | `win`, `taunt` or `intro` |
| `revive` | `h`, `life` | Brings a knocked-out fighter back |
| `palette` | `h`, `pal` | Live palette change |
| `size` | `h`, `scale` | Scale, 0.4 to 2.5 |
| `mute` | `h`, `on` | Silences one fighter |
| `listener` | `h` | Whose position sounds are panned around; -1 for none |
| `floor` | `x0`, `x1` | The floor's x span |
| `corpse_limit` | `limit` | 0 to 128 |
| `clear_leftovers` | | Clears effects in unoccupied slots |
| `pause` | `on` | Pauses the simulation |
| `sound` | `on` | Sound on or off |

Button bits for `input`, from bit 0: `U D L R a b c x y z s d w m`. L and R are absolute, not relative to
facing.

### State record

`get_state()` returns 32 floats per fighter. Positions are in host units, velocities in host units per tick.

| Index | Meaning |
| --- | --- |
| 0 | Handle |
| 1 | Flags: 1 alive, 2 knocked out, 4 has control, 128 in hit pause, 256 guarding |
| 2, 3, 4 | x, depth, height |
| 5, 6, 7 | Velocity: forward, depth, up |
| 8 | Facing, 1 or -1 |
| 9, 10, 11 | Life, max life, power |
| 12, 13, 14 | State number, previous state, time in state |
| 15 | State type: 0 stand, 1 crouch, 2 air, 3 lying |
| 16 | Move type: 0 idle, 1 attack, 2 being hit |
| 17 | Animation number |
| 18 | Focus handle, -1 for none |
| 19 | Team |
| 20 | Last attacker's handle, -1 for none |
| 21 | AI level |
| 22, 23 | Body height and width |
| 24, 25 | Host attack and defense multipliers |
| 26 | 1 if a corpse |
| 27 | Character id, which tells successive fighters of one handle apart |
| 28 | Ticks left of a host freeze or stun |
| 29 | Host motion multiplier |

### Events

`poll_events()` returns dictionaries with an `"e"` key: `spawned`, `spawn_failed`, `despawned`, `hit`,
`damage`, `ko`, `dead`, `corpse`, `corpse_evicted`, `pose_start`, `pose_end`, `controlled`, `revived`,
`command_error`, `shake`, `envcolor`, `allpalfx`, `rumble`, `error`.

A `hit` carries attacker `a`, victim `v`, `dmg`, `guard`, `proj` and the attacker's `state`. Screen shakes
and screen colour flashes arrive as events for your camera and post-processing to act on.

## 5. Test without Godot

`tools/ovhost` is a native test host. It runs a fight through the DLL the way the Godot module does and
writes PNG frames:

```
C:\msys64\usr\bin\bash.exe tools/ovhost/build.sh
ikemen\bin\ovhost.exe --root <repo>/ikemen --p1 D:/packs/mypack/chars/kfm/kfm.def --p2 D:/packs/mypack/chars/kfm/kfm.def --out frames
```

## 6. Parity test

`tools/parity/run.py` plays the same AI-against-AI fight from the same random seed in stock Ikemen GO
v1.0.0 and through `ikemen.dll`, and compares every tick: the state, animation, life, position and velocity
of every character and helper, and the random seed.

```
python tools/parity/run.py --pack D:\packs\mypack --p1 kfm --p2 suave --ticks 3000
```

It downloads and builds stock Ikemen GO on first use (it also needs the MSYS2 package
`mingw-w64-clang-x86_64-ffmpeg`), and the stock executable opens a window for the length of the fight. It
compares simulation state, not pixels or mixed sound. Some packs cannot be compared because stock Ikemen GO
refuses to load them, for example packs that use state controllers of MUGEN hooks.

## Limits

- Windows and the Compatibility renderer only.
- One Ikemen instance per process. The Go runtime cannot be unloaded, so the DLL stays loaded until the
  game exits.
- Keep 2D MSAA off on the viewport that carries the overlay.
- Parity with stock Ikemen GO is tested for the stock two-fighter match (`parity_mode`). The slot mode
  (`ffa`, the default) changes Ikemen's rules on purpose: more than two fighters, teams, depth.

## Making a prebuilt release

A release zip lets people use Godotmen without compiling Godot. After building the editor, the release
template and the DLL:

```
python tools/package.py --src path\to\godot --version 0.1.0
```

This writes `bin/release/godotmen-<version>-windows-x64.zip` with the two Godot executables, `ikemen.dll`
and Ikemen's game folder, the example, this guide, a quick start and the licences. It holds no game content.

Godot's executables contain the full path of the source folder they were built from. Build from a neutral
folder such as `C:\godot-src`, not one under your user profile, before publishing a release.
