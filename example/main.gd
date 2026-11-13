extends Node3D
## Godotmen example: two fighters from a MUGEN / Ikemen pack in a small 3D scene.
##
## Run it with the Godot build that has the ikemen module, and tell it which fighters to load:
##
##   godot --path example -- --p1=D:/packs/mypack/chars/kfm/kfm.def --p2=D:/packs/mypack/chars/kfm/kfm.def
##
## Player 1: A/D walk, W jump, S crouch, J K L and U I O are the six attack buttons. Player 2 is the AI.
## Optional: --ai1=N (0 = you play), --ai2=N, --shot=<file.png> --frames=N (save a screenshot after N ticks and quit).

const METRES_PER_UNIT := 0.015  # one host unit (a pixel of a 320-wide fighter) in the 3D world
const HUD_LAYER := 1

# Ikemen's button bits for the "input" command.
const BIT := {"U": 1, "D": 2, "L": 4, "R": 8, "a": 16, "b": 32, "c": 64, "x": 128, "y": 256, "z": 512}
const KEYS := {
	KEY_W: "U", KEY_S: "D", KEY_A: "L", KEY_D: "R",
	KEY_J: "x", KEY_K: "y", KEY_L: "z", KEY_U: "a", KEY_I: "b", KEY_O: "c",
}

var ik: Object
var camera: Camera3D
var hud: Label
var ready_to_fight := false
var last_bits := -1
var ticks := 0
var args := {}


func _ready() -> void:
	for a in OS.get_cmdline_user_args():
		if a.begins_with("--") and a.contains("="):
			args[a.get_slice("=", 0).substr(2)] = a.substr(a.find("=") + 1)
	_build_scene()

	if not Engine.has_singleton("Ikemen"):
		hud.text = "This Godot build has no ikemen module.\nBuild it with tools/build_godot.py."
		return
	if not args.has("p1") or not args.has("p2"):
		hud.text = "Pass the fighters:\n-- --p1=<path to .def> --p2=<path to .def>"
		return
	ik = Engine.get_singleton("Ikemen")

	# In this repository the Ikemen folder is beside the example. In your own project, put it at res://ikemen and
	# call ik.start("", config).
	var ikemen_root := ProjectSettings.globalize_path("res://").path_join("../ikemen").simplify_path()
	OS.set_environment("IKEMEN_DLL", ikemen_root.path_join("bin/ikemen.dll"))
	var result: int = ik.start(ikemen_root, {
		"fighters": [
			{"def": args["p1"], "pal": 1, "ai": int(args.get("ai1", "0"))},
			{"def": args["p2"], "pal": 2, "ai": int(args.get("ai2", "4"))},
		],
		"stage": "stages/overlay.def",
		"round_time": -1,
		"nosound": args.has("shot"),
	})
	if result < 0:
		hud.text = "Ikemen did not start:\n" + ik.last_error()
		ik = null
		return
	# Ikemen draws after the 3D pass and under canvas layer 1 (the HUD), depth-tested against the 3D scene.
	result = ik.set_overlay(get_tree().root.get_viewport_rid(), HUD_LAYER, ik.DEPTH_TEST)
	if result < 0:
		hud.text = "No overlay:\n" + ik.last_error()


func _build_scene() -> void:
	var env := Environment.new()
	env.background_mode = Environment.BG_COLOR
	env.background_color = Color(0.12, 0.16, 0.26)
	env.ambient_light_source = Environment.AMBIENT_SOURCE_COLOR
	env.ambient_light_color = Color(0.5, 0.5, 0.55)
	var world := WorldEnvironment.new()
	world.environment = env
	add_child(world)

	var light := DirectionalLight3D.new()
	light.rotation_degrees = Vector3(-50, 30, 0)
	add_child(light)

	camera = Camera3D.new()
	camera.fov = 40
	camera.position = Vector3(0, 1.8, 7.5)
	add_child(camera)
	camera.look_at(Vector3(0, 1.1, 0))

	var floor_mesh := MeshInstance3D.new()
	var plane := PlaneMesh.new()
	plane.size = Vector2(24, 8)
	var floor_mat := StandardMaterial3D.new()
	floor_mat.albedo_color = Color(0.32, 0.30, 0.28)
	plane.material = floor_mat
	floor_mesh.mesh = plane
	add_child(floor_mesh)

	# A pillar in front of the fighters' plane and one behind it: the front one must hide them, the back one not.
	for z in [1.2, -1.5]:
		var pillar := MeshInstance3D.new()
		var box := BoxMesh.new()
		box.size = Vector3(0.5, 3.0, 0.5)
		var mat := StandardMaterial3D.new()
		mat.albedo_color = Color(0.55, 0.35, 0.2)
		box.material = mat
		pillar.mesh = box
		pillar.position = Vector3(2.2 if z > 0 else -2.2, 1.5, z)
		add_child(pillar)

	var layer := CanvasLayer.new()
	layer.layer = HUD_LAYER
	add_child(layer)
	hud = Label.new()
	hud.position = Vector2(24, 16)
	hud.add_theme_font_size_override("font_size", 22)
	layer.add_child(hud)


func _physics_process(_delta: float) -> void:
	if ik == null:
		return
	var status: int = ik.status()  # 0 booting, 1 ready, -1 failed
	if status < 0:
		hud.text = "Ikemen failed:\n" + ik.last_error()
		ik = null
		return
	if status == 1:
		ready_to_fight = true
		var bits := 0
		for key in KEYS:
			if Input.is_physical_key_pressed(key):
				bits |= BIT[KEYS[key]]
		if bits != last_bits:
			ik.command({"op": "input", "h": 0, "bits": bits})  # held until changed
			last_bits = bits
	ik.step()  # one 60 Hz Ikemen tick (while booting, this advances the loading)
	if not ready_to_fight:
		hud.text = "Loading..."
		return
	ticks += 1

	# One record of REC_FLOATS floats per fighter: [0] handle, [2] x, [3] depth, [4] height, [9] life, [10] max life.
	var state: PackedFloat32Array = ik.get_state()
	var lines := PackedStringArray()
	var middle := 0.0
	var count := 0
	for i in range(0, state.size(), ik.REC_FLOATS):
		lines.append("Fighter %d   life %d / %d" % [int(state[i]), int(state[i + 9]), int(state[i + 10])])
		middle += state[i + 2] * METRES_PER_UNIT
		count += 1
	for event in ik.poll_events():
		if event.get("e", "") == "ko":
			lines.append("KO")
	hud.text = "\n".join(lines)
	if count > 0:  # the camera follows the fighters' midpoint
		camera.position.x = lerpf(camera.position.x, middle / count, 0.1)

	if args.has("shot") and ticks == int(args.get("frames", "300")):
		await RenderingServer.frame_post_draw
		get_viewport().get_texture().get_image().save_png(args["shot"])
		print("render stats: ", ik.get_render_stats())
		get_tree().quit()


func _process(_delta: float) -> void:
	if ik == null:
		return
	var view_proj := camera.get_camera_projection() * Projection(camera.get_camera_transform().affine_inverse())
	var right := camera.global_transform.basis.x
	# params: [0] world units per host unit, [1] [2] world x / z of host x 0 / depth 0, [3] [4] the direction the
	# sprite cards face along (the camera's right vector on the floor), [5] world y of the floor.
	ik.set_camera(view_proj, PackedFloat32Array([METRES_PER_UNIT, 0.0, 0.0, right.x, right.z, 0.0]))


func _exit_tree() -> void:
	if ik != null:
		ik.shutdown()
