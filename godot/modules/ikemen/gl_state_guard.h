// Godotmen: Ikemen GO overlay module.
// RAII save/restore of every piece of OpenGL state a foreign renderer (Ikemen GO inside Godot's GL context)
// can disturb between Godot's 3D pass and its canvas pass. The state it covers:
// Godot's GLES3 driver re-establishes most state itself, but never re-sets GL_MULTISAMPLE, the front face,
// the colour mask on the canvas path, the clear colour, UNPACK_ROW_LENGTH and friends, so all of them are
// mirrored back here. Cost: ~150 glGet/glSet calls (no GPU sync; NVIDIA threaded optimisation is off for Godot).
#pragma once

#include "platform_gl.h"

class IkemenGLStateGuard {
public:
	// Texture units whose GL_TEXTURE_2D binding is saved and restored. ikemen.dll binds only inside them in overlay
	// mode: its fast path uses units 0/1, and its generic path's LRU texture cache - over every unit the GPU has in
	// stock Ikemen, Godot's fixed units (max-2..max-7) among them - is cut to units 0-7 (ovTexUnits,
	// ikemen/src/host_ov_render.go); unit 8 holds the late pass's copy of the scene depth (ovWorldDepthUnit,
	// host_ov_depth.go). Keep them in step.
	static constexpr int TEXTURE_UNITS = 9;

private:
	GLint program = 0, vao = 0, array_buffer = 0, element_buffer = 0, uniform_buffer = 0;
	GLint pixel_pack_buffer = 0, pixel_unpack_buffer = 0;
	GLint active_texture = GL_TEXTURE0, textures[TEXTURE_UNITS] = {};
	GLint draw_fbo = 0, read_fbo = 0, renderbuffer = 0;
	GLint viewport[4] = {}, scissor_box[4] = {};
	GLint blend_eq_rgb = GL_FUNC_ADD, blend_eq_alpha = GL_FUNC_ADD;
	GLint blend_src_rgb = GL_ONE, blend_dst_rgb = GL_ZERO, blend_src_alpha = GL_ONE, blend_dst_alpha = GL_ZERO;
	GLfloat blend_color[4] = {};
	GLint front_face = GL_CCW, cull_face_mode = GL_BACK, depth_func = GL_LESS;
	GLint stencil_func = GL_ALWAYS, stencil_ref = 0, stencil_value_mask = -1, stencil_writemask = -1;
	GLint stencil_fail = GL_KEEP, stencil_zfail = GL_KEEP, stencil_zpass = GL_KEEP;
	GLint stencil_back_func = GL_ALWAYS, stencil_back_ref = 0, stencil_back_value_mask = -1, stencil_back_writemask = -1;
	GLint stencil_back_fail = GL_KEEP, stencil_back_zfail = GL_KEEP, stencil_back_zpass = GL_KEEP;
	GLint unpack_alignment = 4, unpack_row_length = 0, unpack_skip_rows = 0, unpack_skip_pixels = 0;
	GLint unpack_image_height = 0, unpack_skip_images = 0, pack_alignment = 4, pack_row_length = 0;
	GLint pack_skip_rows = 0, pack_skip_pixels = 0;
	GLboolean depth_mask = GL_TRUE, color_mask[4] = { GL_TRUE, GL_TRUE, GL_TRUE, GL_TRUE };
	GLfloat clear_color[4] = {}, clear_depth = 1.0f;
	GLint clear_stencil = 0;
	GLfloat polygon_offset_factor = 0.0f, polygon_offset_units = 0.0f;
	GLfloat line_width = 1.0f;

	struct Cap {
		GLenum cap;
		GLboolean on;
	};
	Cap caps[14] = {
		{ GL_BLEND, GL_FALSE },
		{ GL_SCISSOR_TEST, GL_FALSE },
		{ GL_DEPTH_TEST, GL_FALSE },
		{ GL_CULL_FACE, GL_FALSE },
		{ GL_MULTISAMPLE, GL_TRUE },
		{ GL_STENCIL_TEST, GL_FALSE },
		{ GL_SAMPLE_ALPHA_TO_COVERAGE, GL_FALSE },
		{ GL_POLYGON_OFFSET_FILL, GL_FALSE },
		{ GL_FRAMEBUFFER_SRGB, GL_FALSE },
		{ GL_DITHER, GL_TRUE },
		{ GL_PRIMITIVE_RESTART, GL_FALSE },
		{ GL_PROGRAM_POINT_SIZE, GL_FALSE },
		{ GL_RASTERIZER_DISCARD, GL_FALSE },
		{ GL_DEPTH_CLAMP, GL_FALSE },
	};

	static void set_cap(GLenum p_cap, GLboolean p_on) {
		if (p_on) {
			glEnable(p_cap);
		} else {
			glDisable(p_cap);
		}
	}

public:
	IkemenGLStateGuard() {
		glGetIntegerv(GL_CURRENT_PROGRAM, &program);
		glGetIntegerv(GL_VERTEX_ARRAY_BINDING, &vao);
		glGetIntegerv(GL_ARRAY_BUFFER_BINDING, &array_buffer);
		glGetIntegerv(GL_ELEMENT_ARRAY_BUFFER_BINDING, &element_buffer);
		glGetIntegerv(GL_UNIFORM_BUFFER_BINDING, &uniform_buffer);
		glGetIntegerv(GL_PIXEL_PACK_BUFFER_BINDING, &pixel_pack_buffer);
		glGetIntegerv(GL_PIXEL_UNPACK_BUFFER_BINDING, &pixel_unpack_buffer);
		glGetIntegerv(GL_DRAW_FRAMEBUFFER_BINDING, &draw_fbo);
		glGetIntegerv(GL_READ_FRAMEBUFFER_BINDING, &read_fbo);
		glGetIntegerv(GL_RENDERBUFFER_BINDING, &renderbuffer);
		glGetIntegerv(GL_VIEWPORT, viewport);
		glGetIntegerv(GL_SCISSOR_BOX, scissor_box);

		glGetIntegerv(GL_BLEND_EQUATION_RGB, &blend_eq_rgb);
		glGetIntegerv(GL_BLEND_EQUATION_ALPHA, &blend_eq_alpha);
		glGetIntegerv(GL_BLEND_SRC_RGB, &blend_src_rgb);
		glGetIntegerv(GL_BLEND_DST_RGB, &blend_dst_rgb);
		glGetIntegerv(GL_BLEND_SRC_ALPHA, &blend_src_alpha);
		glGetIntegerv(GL_BLEND_DST_ALPHA, &blend_dst_alpha);
		glGetFloatv(GL_BLEND_COLOR, blend_color);

		glGetIntegerv(GL_FRONT_FACE, &front_face);
		glGetIntegerv(GL_CULL_FACE_MODE, &cull_face_mode);
		glGetIntegerv(GL_DEPTH_FUNC, &depth_func);
		glGetBooleanv(GL_DEPTH_WRITEMASK, &depth_mask);
		glGetBooleanv(GL_COLOR_WRITEMASK, color_mask);

		glGetIntegerv(GL_STENCIL_FUNC, &stencil_func);
		glGetIntegerv(GL_STENCIL_REF, &stencil_ref);
		glGetIntegerv(GL_STENCIL_VALUE_MASK, &stencil_value_mask);
		glGetIntegerv(GL_STENCIL_WRITEMASK, &stencil_writemask);
		glGetIntegerv(GL_STENCIL_FAIL, &stencil_fail);
		glGetIntegerv(GL_STENCIL_PASS_DEPTH_FAIL, &stencil_zfail);
		glGetIntegerv(GL_STENCIL_PASS_DEPTH_PASS, &stencil_zpass);
		glGetIntegerv(GL_STENCIL_BACK_FUNC, &stencil_back_func);
		glGetIntegerv(GL_STENCIL_BACK_REF, &stencil_back_ref);
		glGetIntegerv(GL_STENCIL_BACK_VALUE_MASK, &stencil_back_value_mask);
		glGetIntegerv(GL_STENCIL_BACK_WRITEMASK, &stencil_back_writemask);
		glGetIntegerv(GL_STENCIL_BACK_FAIL, &stencil_back_fail);
		glGetIntegerv(GL_STENCIL_BACK_PASS_DEPTH_FAIL, &stencil_back_zfail);
		glGetIntegerv(GL_STENCIL_BACK_PASS_DEPTH_PASS, &stencil_back_zpass);

		glGetIntegerv(GL_UNPACK_ALIGNMENT, &unpack_alignment);
		glGetIntegerv(GL_UNPACK_ROW_LENGTH, &unpack_row_length);
		glGetIntegerv(GL_UNPACK_SKIP_ROWS, &unpack_skip_rows);
		glGetIntegerv(GL_UNPACK_SKIP_PIXELS, &unpack_skip_pixels);
		glGetIntegerv(GL_UNPACK_IMAGE_HEIGHT, &unpack_image_height);
		glGetIntegerv(GL_UNPACK_SKIP_IMAGES, &unpack_skip_images);
		glGetIntegerv(GL_PACK_ALIGNMENT, &pack_alignment);
		glGetIntegerv(GL_PACK_ROW_LENGTH, &pack_row_length);
		glGetIntegerv(GL_PACK_SKIP_ROWS, &pack_skip_rows);
		glGetIntegerv(GL_PACK_SKIP_PIXELS, &pack_skip_pixels);

		glGetFloatv(GL_COLOR_CLEAR_VALUE, clear_color);
		glGetFloatv(GL_DEPTH_CLEAR_VALUE, &clear_depth);
		glGetIntegerv(GL_STENCIL_CLEAR_VALUE, &clear_stencil);
		glGetFloatv(GL_POLYGON_OFFSET_FACTOR, &polygon_offset_factor);
		glGetFloatv(GL_POLYGON_OFFSET_UNITS, &polygon_offset_units);
		glGetFloatv(GL_LINE_WIDTH, &line_width);

		for (Cap &c : caps) {
			c.on = glIsEnabled(c.cap);
		}

		glGetIntegerv(GL_ACTIVE_TEXTURE, &active_texture);
		for (int i = 0; i < TEXTURE_UNITS; i++) {
			glActiveTexture(GL_TEXTURE0 + i);
			glGetIntegerv(GL_TEXTURE_BINDING_2D, &textures[i]);
		}
		glActiveTexture(active_texture);
	}

	~IkemenGLStateGuard() {
		glBindFramebuffer(GL_DRAW_FRAMEBUFFER, draw_fbo);
		glBindFramebuffer(GL_READ_FRAMEBUFFER, read_fbo);
		glBindRenderbuffer(GL_RENDERBUFFER, renderbuffer);
		glUseProgram(program);
		glBindVertexArray(vao);
		// Element binding is VAO state: re-binding the saved value repairs a VAO the foreign code touched.
		glBindBuffer(GL_ELEMENT_ARRAY_BUFFER, element_buffer);
		glBindBuffer(GL_ARRAY_BUFFER, array_buffer);
		glBindBuffer(GL_UNIFORM_BUFFER, uniform_buffer);
		glBindBuffer(GL_PIXEL_PACK_BUFFER, pixel_pack_buffer);
		glBindBuffer(GL_PIXEL_UNPACK_BUFFER, pixel_unpack_buffer);
		glViewport(viewport[0], viewport[1], viewport[2], viewport[3]);
		glScissor(scissor_box[0], scissor_box[1], scissor_box[2], scissor_box[3]);

		glBlendEquationSeparate(blend_eq_rgb, blend_eq_alpha);
		glBlendFuncSeparate(blend_src_rgb, blend_dst_rgb, blend_src_alpha, blend_dst_alpha);
		glBlendColor(blend_color[0], blend_color[1], blend_color[2], blend_color[3]);

		glFrontFace(front_face);
		glCullFace(cull_face_mode);
		glDepthFunc(depth_func);
		glDepthMask(depth_mask);
		glColorMask(color_mask[0], color_mask[1], color_mask[2], color_mask[3]);

		glStencilFuncSeparate(GL_FRONT, stencil_func, stencil_ref, (GLuint)stencil_value_mask);
		glStencilFuncSeparate(GL_BACK, stencil_back_func, stencil_back_ref, (GLuint)stencil_back_value_mask);
		glStencilMaskSeparate(GL_FRONT, (GLuint)stencil_writemask);
		glStencilMaskSeparate(GL_BACK, (GLuint)stencil_back_writemask);
		glStencilOpSeparate(GL_FRONT, stencil_fail, stencil_zfail, stencil_zpass);
		glStencilOpSeparate(GL_BACK, stencil_back_fail, stencil_back_zfail, stencil_back_zpass);

		glPixelStorei(GL_UNPACK_ALIGNMENT, unpack_alignment);
		glPixelStorei(GL_UNPACK_ROW_LENGTH, unpack_row_length);
		glPixelStorei(GL_UNPACK_SKIP_ROWS, unpack_skip_rows);
		glPixelStorei(GL_UNPACK_SKIP_PIXELS, unpack_skip_pixels);
		glPixelStorei(GL_UNPACK_IMAGE_HEIGHT, unpack_image_height);
		glPixelStorei(GL_UNPACK_SKIP_IMAGES, unpack_skip_images);
		glPixelStorei(GL_PACK_ALIGNMENT, pack_alignment);
		glPixelStorei(GL_PACK_ROW_LENGTH, pack_row_length);
		glPixelStorei(GL_PACK_SKIP_ROWS, pack_skip_rows);
		glPixelStorei(GL_PACK_SKIP_PIXELS, pack_skip_pixels);

		glClearColor(clear_color[0], clear_color[1], clear_color[2], clear_color[3]);
		glClearDepth(clear_depth);
		glClearStencil(clear_stencil);
		glPolygonOffset(polygon_offset_factor, polygon_offset_units);
		glLineWidth(line_width);

		for (const Cap &c : caps) {
			set_cap(c.cap, c.on);
		}

		for (int i = TEXTURE_UNITS - 1; i >= 0; i--) {
			glActiveTexture(GL_TEXTURE0 + i);
			glBindTexture(GL_TEXTURE_2D, textures[i]);
		}
		glActiveTexture(active_texture);
	}

	IkemenGLStateGuard(const IkemenGLStateGuard &) = delete;
	IkemenGLStateGuard &operator=(const IkemenGLStateGuard &) = delete;
};
