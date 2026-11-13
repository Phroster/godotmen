#if __VERSION__ >= 450
	// VULKAN PATH
	#define COMPAT_TEXTURE texture
	layout(binding = 1) uniform UniformBufferObject  {
		vec4 x1x2x4x3;
		vec4 tint;
		vec3 add;
		vec3 mult;
		float alpha, gray, hue;
		int mask;
		bool isFlat, isRgba, isTrapez, neg;
	};
	layout(push_constant, std430) uniform u {
		vec4 palUV;
	};
	layout(binding = 2) uniform sampler2D tex;
	layout(binding = 3) uniform sampler2D pal;
	layout(location = 0) in vec2 texcoord;
	layout(location = 0) out vec4 FragColor;
#else
	// OPENGL / GLES PATH
	#define COMPAT_VARYING in
	#define COMPAT_TEXTURE texture
	#ifdef GL_ES
		precision highp float;
		precision highp int;
	#endif
	out vec4 FragColor;

	uniform sampler2D tex;
	uniform sampler2D pal;
	uniform vec4 x1x2x4x3;
	uniform vec4 tint;
	uniform vec3 add, mult;
	uniform float alpha, gray, hue;
	uniform int mask;
	uniform bool isFlat, isRgba, isTrapez, neg;
	// Overlay (3D cards): the floor rule's window-space depth plane (depth = x*X + y*Y + z) and its sign in w
	// (+1 reverse-Z host: nearer = larger, -1 standard depth, 0 off - the default, so stock is untouched)
	uniform vec4 ovFloor;
	// Overlay, the late pass (host_ov_depth.go): the depth test against the host's scene made here, against its
	// depth as it was before any sprite (ovWorldDepth) - 1 GEQUAL (reverse-Z host), -1 LEQUAL, 0 the hardware's
	// (the default) - and ovDepthCut 1: an opaque pass, whose see-through texels leave no depth
	uniform int ovDepthTest;
	uniform int ovDepthCut;
	uniform sampler2D ovWorldDepth;
	// ...and the depth an opaque pass leaves: the window-space plane of an upright billboard at the sprite's anchor
	// (host_ov_depth.go setUpright), never behind the scene; ovUprightAt: that billboard at the anchor itself - what
	// a standing sprite is tested against the scene with (w 0: its card)
	uniform vec4 ovUpright;
	uniform vec4 ovUprightAt;
	// Overlay: 1 - a magnified indexed sprite's texels are drawn whole with their edges antialiased (ovTexelAA);
	// 0 - the default - the nearest texel, as stock
	uniform int ovSmooth;
	COMPAT_VARYING vec2 texcoord;
#endif

#if __VERSION__ < 450
// Overlay: one texel's colour (the palette's for an indexed sprite), straight alpha
vec4 ovTexel(vec2 uv) {
	vec4 c = COMPAT_TEXTURE(tex, uv);
	if (!isRgba) {
		c = COMPAT_TEXTURE(pal, vec2(c.r*0.9966, 0.5));
	}
	if (mask == -1) c.a = 1.0;
	return c;
}

// Overlay: the texel at uv drawn whole, its edges antialiased (ovSmooth 1). Magnified, each texel's own colour fills
// it but for a pixel's width at its edges, where it blends with its neighbour's by as much of the pixel as lies over
// that one - so as the camera moves and a card's scale and place change by fractions of a pixel its texels slide with
// it instead of jumping whole pixels. w: texels a pixel (fwidth, taken where every pixel of the quad runs it). Colour
// and coverage blend premultiplied - an edge against nothing fades out, never darkens - and come back straight.
// Minified: the nearest texel.
vec4 ovAATap(vec2 cell, vec2 size) {
	if (cell.x < 0.0 || cell.y < 0.0 || cell.x >= size.x || cell.y >= size.y) return vec4(0.0);
	vec4 t = ovTexel((cell + 0.5) / size);
	return vec4(t.rgb * t.a, t.a);
}
vec4 ovTexelAA(vec2 uv, vec2 w) {
	if (w.x >= 1.0 || w.y >= 1.0 || w.x <= 0.0 || w.y <= 0.0) return ovTexel(uv);
	vec2 size = vec2(textureSize(tex, 0));
	vec2 q = uv * size - 0.5;
	vec2 i0 = floor(q);
	vec2 k = clamp((q - i0 - 0.5) / w + 0.5, 0.0, 1.0); // (the far texel's share: 0 or 1 but within half a pixel of the edge)
	bool ex = k.x > 0.0 && k.x < 1.0;
	bool ey = k.y > 0.0 && k.y < 1.0;
	if (!ex && !ey) return ovTexel(uv); // (inside one texel: that texel, exactly)
	vec2 c0 = i0 + floor(k); // (the texel on the near side of each edge it is not on)
	vec4 m;
	if (ex && ey) {
		m = mix(mix(ovAATap(i0, size), ovAATap(i0 + vec2(1.0, 0.0), size), k.x),
			mix(ovAATap(i0 + vec2(0.0, 1.0), size), ovAATap(i0 + vec2(1.0, 1.0), size), k.x), k.y);
	} else if (ex) {
		m = mix(ovAATap(vec2(i0.x, c0.y), size), ovAATap(vec2(i0.x + 1.0, c0.y), size), k.x);
	} else {
		m = mix(ovAATap(vec2(c0.x, i0.y), size), ovAATap(vec2(c0.x, i0.y + 1.0), size), k.y);
	}
	return m.a > 0.0001 ? vec4(m.rgb / m.a, m.a) : vec4(0.0);
}
#endif

vec3 rgb2hsv(vec3 c)
{
    vec4 K = vec4(0.0, -1.0 / 3.0, 2.0 / 3.0, -1.0);
    vec4 p = mix(vec4(c.bg, K.wz), vec4(c.gb, K.xy), step(c.b, c.g));
    vec4 q = mix(vec4(p.xyw, c.r), vec4(c.r, p.yzx), step(p.x, c.r));

    float d = q.x - min(q.w, q.y);
    float e = 1.0e-10;
    return vec3(abs(q.z + (q.w - q.y) / (6.0 * d + e)), d / (q.x + e), q.x);
}

vec3 hsv2rgb(vec3 c)
{
    vec4 K = vec4(1.0, 2.0 / 3.0, 1.0 / 3.0, 3.0);
    vec3 p = abs(fract(c.xxx + K.xyz) * 6.0 - K.www);
    return c.z * mix(K.xxx, clamp(p - K.xxx, 0.0, 1.0), c.y);
}

vec3 hue_shift(vec3 color, float dhue) {
	vec3 colorhsv = rgb2hsv(color);
	colorhsv.x = mod(colorhsv.x+dhue, 1.0);
	return hsv2rgb(colorhsv);
}

void main(void) {
	vec4 c;
	vec3 neg_base = vec3(1.0);
	vec3 final_add = add;
	vec4 final_mul = vec4(mult, alpha);

	// Select flat color or textures
	if (isFlat) {
		c = tint; 

		// Treat flat colors like RGBA for math consistency
		neg_base *= c.a;
		final_add *= c.a;
		final_mul.rgb *= alpha;
	} else {
		vec2 uv = texcoord;
		if (isTrapez) {
			vec2 bounds = mix(x1x2x4x3.zw, x1x2x4x3.xy, uv.y);
			float gap = bounds[1] - bounds[0];
			#ifdef GL_ES
				if (abs(gap) < 0.0001) gap = 0.0001;
			#endif
			uv.x = (gl_FragCoord.x - bounds[0]) / gap;
		}

		#if __VERSION__ < 450
			// (an RGBA sprite's colours are premultiplied: sampled as stock)
			bool ovAA = ovSmooth == 1 && !isRgba;
			vec2 ovAAw = fwidth(uv * vec2(textureSize(tex, 0)));
			if (ovAA) {
				c = ovTexelAA(uv, ovAAw);
			} else {
				c = COMPAT_TEXTURE(tex, uv);
			}
		#else
			bool ovAA = false;
			c = COMPAT_TEXTURE(tex, uv);
		#endif

		// Select with or without palette
		if (ovAA) {
			// (already coloured: ovTexelAA)
		} else if (isRgba) {
			if (mask == -1) c.a = 1.0;
			neg_base *= c.a;
			final_add *= c.a;
			final_mul.rgb *= alpha;
		} else {
			// Palette lookup
			#if __VERSION__ >= 450
				c = COMPAT_TEXTURE(pal, vec2(palUV[0]+palUV[2]*c.r*0.9966, palUV[1]));
			#else
				c = COMPAT_TEXTURE(pal, vec2(c.r*0.9966, 0.5));
			#endif
			if (mask == -1) c.a = 1.0;
		}
	}

	// Apply PalFX
	// Hue
	if (hue != 0.0) {
		c.rgb = hue_shift(c.rgb, hue);
	}
	// Invertall
	if (neg) {
		c.rgb = neg_base - c.rgb;
	}
	// Color
	c.rgb = mix(vec3((c.r + c.g + c.b) / 3.0), c.rgb, 1.0 - gray);
	// Add
	c.rgb += final_add;
	// Mul
	c *= final_mul;

	// Apply tint
	// Sprites only, because flat colors are already tinted
	if (!isFlat) {
		c.rgb = mix(c.rgb, tint.rgb * c.a, tint.a);
	}

	FragColor = c;

	#if __VERSION__ < 450
		// Overlay: the depth the test uses is the nearer of the card's and the (lifted) floor's at this pixel - what
		// lies below a sprite's ground line is tested as lying on the floor in front of its card (host_ov_frame.go
		// setFloor). Colour is untouched; with ovFloor.w = 0 this is the default depth.
		float ovFloorDepth = dot(ovFloor.xyz, vec3(gl_FragCoord.xy, 1.0));
		float ovDepth = gl_FragCoord.z;
		if (ovFloor.w > 0.5) {
			ovDepth = max(gl_FragCoord.z, ovFloorDepth);
		} else if (ovFloor.w < -0.5) {
			ovDepth = min(gl_FragCoord.z, ovFloorDepth);
		}
		if (ovDepthTest != 0) {
			// (the late pass: the hardware's test always passes - the sprites keep MUGEN's layering among themselves -
			// and an opaque pass writes its depth for the host's transparent objects drawn after: host_ov_depth.go)
			float ovScene = texelFetch(ovWorldDepth, ivec2(gl_FragCoord.xy), 0).r;
			// (a standing sprite is tested as the upright billboard at its anchor, never its card leaning back with
			// the camera; below its ground line, the floor rule's)
			float ovTest = ovDepth;
			if (ovUprightAt.w != 0.0) {
				float ovAt = dot(ovUprightAt.xyz, vec3(gl_FragCoord.xy, 1.0));
				ovTest = ovFloor.w > 0.5 ? max(ovAt, ovFloorDepth) : (ovFloor.w < -0.5 ? min(ovAt, ovFloorDepth) : ovAt);
			}
			if (ovDepthTest > 0 ? ovTest < ovScene : ovTest > ovScene) {
				discard;
			}
			if (ovDepthCut >= 1 && FragColor.a < 0.5) {
				discard;
			}
			if (ovDepthCut >= 1 && ovUpright.w != 0.0) {
				float ovStand = dot(ovUpright.xyz, vec3(gl_FragCoord.xy, 1.0));
				ovDepth = ovDepthTest > 0 ? max(ovStand, ovScene) : min(ovStand, ovScene);
			}
			if (ovDepthCut == 2) {
				FragColor = vec4(1.0, 0.0, 1.0, 1.0); // (dev: the texels that leave their depth, magenta)
			}
		}
		gl_FragDepth = ovDepth;
	#endif
}
