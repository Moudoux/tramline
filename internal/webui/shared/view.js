// Tramline's 3D print view, shared by the read-only page and the GUI. No libraries: raw
// WebGL 2, as the pages are embedded in the binary and Tramline has no outside dependencies.
"use strict";

// ---- What the printer's position is based on ----
//
// Nothing here measures the head. The server knows how far through the file the print
// has got, and the page works out where that puts the head from the file's own moves.
//
// The file position is ahead of the head: lines wait in the firmware's queues before
// they move anything. A stream sends one line at a time, so about the planner's 16 moves
// are ahead. From the card, the firmware also reads ahead into its 16-line command queue.
const LAG_MOVES = { stream: 16, sd: 32 };
// A position from the card is only asked for about every 30 s, and the answer can take a
// while. In between the page carries on at the file's speeds, for this long at most.
const SD_COAST_SECONDS = 50;
// Further than this from where it should be, the head jumps there rather than catching up.
const SNAP_SECONDS = 30;
// A big model is a lot to redraw for a head that has moved a little, and on a phone that
// means battery. Turning the view always redraws at once.
const MIN_DRAW_MS = window.matchMedia("(pointer: coarse)").matches ? 100 : 40;
// A tap is short and doesn't wander, and two of them close together make a double-tap.
const TAP_MS = 300;
const TAP_PIXELS = 10;
const DOUBLE_TAP_MS = 350;
const MIN_DISTANCE = 15;
const MAX_DISTANCE = 1500;

const BED = 220;
const FILAMENT_WIDTH = 0.45;

// ---- Small matrix helpers, column-major like WebGL ----

function perspective(fovy, aspect, near, far) {
  const f = 1 / Math.tan(fovy / 2);
  return new Float32Array([
    f / aspect, 0, 0, 0,
    0, f, 0, 0,
    0, 0, (far + near) / (near - far), -1,
    0, 0, (2 * far * near) / (near - far), 0,
  ]);
}

const sub = (a, b) => [a[0] - b[0], a[1] - b[1], a[2] - b[2]];
const cross = (a, b) => [a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0]];
const dot = (a, b) => a[0] * b[0] + a[1] * b[1] + a[2] * b[2];
const unit = (a) => {
  const l = Math.hypot(a[0], a[1], a[2]) || 1;
  return [a[0] / l, a[1] / l, a[2] / l];
};

function lookAt(eye, target, up) {
  const z = unit(sub(eye, target));
  const x = unit(cross(up, z));
  const y = cross(z, x);
  return new Float32Array([
    x[0], y[0], z[0], 0,
    x[1], y[1], z[1], 0,
    x[2], y[2], z[2], 0,
    -dot(x, eye), -dot(y, eye), -dot(z, eye), 1,
  ]);
}

function multiply(a, b) {
  const out = new Float32Array(16);
  for (let c = 0; c < 4; c++) {
    for (let r = 0; r < 4; r++) {
      let sum = 0;
      for (let k = 0; k < 4; k++) sum += a[k * 4 + r] * b[c * 4 + k];
      out[c * 4 + r] = sum;
    }
  }
  return out;
}

// ---- The model: what the server's binary form holds ----

class Model {
  constructor(buffer) {
    const head = new DataView(buffer);
    const magic = String.fromCharCode(head.getUint8(0), head.getUint8(1), head.getUint8(2), head.getUint8(3));
    if (magic !== "TLP1") throw new Error("not a Tramline path");
    const count = head.getUint32(4, true);
    this.count = count;
    this.seconds = head.getFloat32(8, true);
    const floats = new Float32Array(buffer, 12, count * 6);
    const words = new Uint32Array(buffer, 12, count * 6);

    // Positions and times start from the moment before each move, so index i is the
    // move's start and i+1 its end, with the head starting at the origin.
    this.xyz = new Float32Array((count + 1) * 3);
    this.time = new Float32Array(count + 1);
    this.end = new Uint32Array(count);
    // How many plastic segments come before a move, and those segments for the GPU.
    this.segBefore = new Uint32Array(count + 2);
    let segments = 0;
    for (let i = 0; i < count; i++) if (words[i * 6 + 5] & 1) segments++;
    this.segments = new Float32Array(segments * 6);
    // The layer of each move, counted as a rise in the height of plastic.
    this.layerOf = new Uint32Array(count + 1);

    let seg = 0;
    let layer = 0;
    let top = -Infinity;
    const min = [Infinity, Infinity, Infinity];
    const max = [-Infinity, -Infinity, -Infinity];
    for (let i = 0; i < count; i++) {
      const at = i * 6;
      const [x, y, z] = [floats[at], floats[at + 1], floats[at + 2]];
      this.xyz.set([x, y, z], (i + 1) * 3);
      this.time[i + 1] = floats[at + 3];
      this.end[i] = words[at + 4];
      this.segBefore[i] = seg;
      if (words[at + 5] & 1) {
        this.segments.set([this.xyz[i * 3], this.xyz[i * 3 + 1], this.xyz[i * 3 + 2], x, y, z], seg * 6);
        seg++;
        if (z > top + 1e-4) {
          top = z;
          layer++;
        }
        min[0] = Math.min(min[0], x); min[1] = Math.min(min[1], y); min[2] = Math.min(min[2], z);
        max[0] = Math.max(max[0], x); max[1] = Math.max(max[1], y); max[2] = Math.max(max[2], z);
      }
      this.layerOf[i + 1] = layer;
    }
    this.segBefore[count] = this.segBefore[count + 1] = seg;
    this.segmentCount = seg;
    this.layers = layer;
    this.center = seg ? [(min[0] + max[0]) / 2, (min[1] + max[1]) / 2, (min[2] + max[2]) / 2] : [BED / 2, BED / 2, 0];
    this.radius = seg ? Math.max(Math.hypot(max[0] - min[0], max[1] - min[1], max[2] - min[2]) / 2, 20) : BED / 2;
  }

  // The first move whose line ends at or after a file position.
  moveAtPos(pos) {
    let lo = 0;
    let hi = this.count;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.end[mid] < pos) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  // The move in progress at a time along the file.
  moveAtTime(t) {
    let lo = 0;
    let hi = this.count;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.time[mid + 1] < t) lo = mid + 1;
      else hi = mid;
    }
    return Math.min(lo, Math.max(this.count - 1, 0));
  }

  // Where the head is at a time along the file, and how much is drawn behind it.
  at(t) {
    if (this.count === 0) return null;
    const i = this.moveAtTime(t);
    const start = this.time[i];
    const span = this.time[i + 1] - start;
    const f = span > 0 ? Math.min(Math.max((t - start) / span, 0), 1) : 1;
    const from = i * 3;
    const to = from + 3;
    const xyz = this.xyz;
    const pos = [
      xyz[from] + (xyz[to] - xyz[from]) * f,
      xyz[from + 1] + (xyz[to + 1] - xyz[from + 1]) * f,
      xyz[from + 2] + (xyz[to + 2] - xyz[from + 2]) * f,
    ];
    const drawing = this.segBefore[i + 1] > this.segBefore[i];
    return {
      pos,
      done: this.segBefore[i],
      partial: drawing ? [xyz[from], xyz[from + 1], xyz[from + 2], pos[0], pos[1], pos[2]] : null,
      layer: this.layerOf[i + 1],
    };
  }
}

// ---- Drawing ----

const RIBBON_VS = `#version 300 es
in vec3 a_from;
in vec3 a_to;
in vec2 a_corner;
uniform mat4 u_viewProj;
uniform vec3 u_eye;
uniform float u_width;
uniform float u_pixel;
out vec3 v_side;
out vec3 v_toEye;
out float v_across;
out float v_z;
void main() {
  vec3 dir = a_to - a_from;
  float len = length(dir);
  dir = len > 0.0 ? dir / len : vec3(1.0, 0.0, 0.0);
  vec3 p = mix(a_from, a_to, a_corner.x);
  vec3 toEye = u_eye - p;
  vec3 side = cross(dir, toEye);
  float sideLen = length(side);
  side = sideLen > 1e-5 ? side / sideLen : vec3(0.0, 0.0, 1.0);
  // Never thinner than a pixel, so a far-off model doesn't break up.
  float half_ = max(u_width * 0.5, u_pixel * length(toEye) * 0.6);
  p += side * a_corner.y * half_;
  p += dir * (a_corner.x * 2.0 - 1.0) * half_;
  v_side = side;
  v_toEye = toEye;
  v_across = a_corner.y;
  v_z = a_from.z;
  gl_Position = u_viewProj * vec4(p, 1.0);
}`;

// Each ribbon is shaded as if it were a round bead of plastic.
const RIBBON_FS = `#version 300 es
precision highp float;
in vec3 v_side;
in vec3 v_toEye;
in float v_across;
in float v_z;
uniform vec4 u_color;
out vec4 color;
void main() {
  vec3 toEye = normalize(v_toEye);
  vec3 side = normalize(v_side);
  vec3 facing = normalize(toEye - side * dot(toEye, side));
  vec3 n = side * v_across + facing * sqrt(max(1.0 - v_across * v_across, 0.0));
  float key = max(dot(n, normalize(vec3(0.4, -0.5, 0.8))), 0.0);
  float fill = max(dot(n, normalize(vec3(-0.6, 0.4, 0.3))), 0.0);
  float layerBand = 0.97 + 0.03 * sin(v_z * 12.0);
  float light = (0.32 + 0.62 * key + 0.16 * fill) * layerBand;
  color = vec4(u_color.rgb * light, u_color.a);
}`;

const FLAT_VS = `#version 300 es
in vec3 a_pos;
uniform mat4 u_viewProj;
void main() { gl_Position = u_viewProj * vec4(a_pos, 1.0); }`;

const FLAT_FS = `#version 300 es
precision highp float;
uniform vec4 u_color;
out vec4 color;
void main() { color = u_color; }`;

// Solid shapes for the tram view: a unit mesh scaled, turned and moved into place, lit by its
// faces' own slopes, so it needs no normals.
const LIT_VS = `#version 300 es
in vec3 a_pos;
uniform mat4 u_viewProj;
uniform vec3 u_offset;
uniform vec3 u_scale;
uniform vec2 u_turn;
out vec3 v_pos;
void main() {
  vec3 p = a_pos * u_scale;
  p.xy = vec2(p.x * u_turn.x - p.y * u_turn.y, p.x * u_turn.y + p.y * u_turn.x);
  v_pos = p + u_offset;
  gl_Position = u_viewProj * vec4(v_pos, 1.0);
}`;

const LIT_FS = `#version 300 es
precision highp float;
in vec3 v_pos;
uniform vec4 u_color;
out vec4 color;
void main() {
  vec3 n = normalize(cross(dFdx(v_pos), dFdy(v_pos)));
  float key = abs(dot(n, normalize(vec3(0.4, -0.5, 0.8))));
  float fill = abs(dot(n, normalize(vec3(-0.6, 0.4, 0.3))));
  color = vec4(u_color.rgb * (0.38 + 0.55 * key + 0.15 * fill), u_color.a);
}`;

// The mesh's surface: each corner has its own colour, lit by the faces' slopes like LIT.
const SURF_VS = `#version 300 es
in vec3 a_pos;
in vec3 a_color;
uniform mat4 u_viewProj;
out vec3 v_pos;
out vec3 v_color;
void main() {
  v_pos = a_pos;
  v_color = a_color;
  gl_Position = u_viewProj * vec4(a_pos, 1.0);
}`;

const SURF_FS = `#version 300 es
precision highp float;
in vec3 v_pos;
in vec3 v_color;
uniform float u_alpha;
out vec4 color;
void main() {
  vec3 n = normalize(cross(dFdx(v_pos), dFdy(v_pos)));
  float key = abs(dot(n, normalize(vec3(0.4, -0.5, 0.8))));
  float fill = abs(dot(n, normalize(vec3(-0.6, 0.4, 0.3))));
  color = vec4(v_color * (0.45 + 0.45 * key + 0.15 * fill), u_alpha);
}`;

function program(gl, vs, fs) {
  const compile = (type, src) => {
    const sh = gl.createShader(type);
    gl.shaderSource(sh, src);
    gl.compileShader(sh);
    if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) throw new Error(gl.getShaderInfoLog(sh));
    return sh;
  };
  const p = gl.createProgram();
  gl.attachShader(p, compile(gl.VERTEX_SHADER, vs));
  gl.attachShader(p, compile(gl.FRAGMENT_SHADER, fs));
  gl.linkProgram(p);
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) throw new Error(gl.getProgramInfoLog(p));
  const uniforms = {};
  const n = gl.getProgramParameter(p, gl.ACTIVE_UNIFORMS);
  for (let i = 0; i < n; i++) {
    const name = gl.getActiveUniform(p, i).name;
    uniforms[name] = gl.getUniformLocation(p, name);
  }
  return { p, uniforms };
}

class Renderer {
  constructor(canvas) {
    this.canvas = canvas;
    const gl = canvas.getContext("webgl2", { antialias: true, alpha: false });
    if (!gl) throw new Error("no WebGL 2");
    this.gl = gl;
    this.ribbon = program(gl, RIBBON_VS, RIBBON_FS);
    this.flat = program(gl, FLAT_VS, FLAT_FS);
    this.lit = program(gl, LIT_VS, LIT_FS);
    this.surf = program(gl, SURF_VS, SURF_FS);
    this.surfVao = null;
    this.surfData = null;
    gl.enable(gl.DEPTH_TEST);
    gl.clearColor(0.082, 0.094, 0.11, 1);
    gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);

    // The camera turns around a target, in spherical terms, with Z up.
    this.home = { target: [BED / 2, BED / 2, 0], distance: 380, yaw: -0.65, pitch: 0.75 };
    this.reset();

    this.corner = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.corner);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([0, -1, 0, 1, 1, -1, 1, 1]), gl.STATIC_DRAW);

    this.segBuffer = gl.createBuffer();
    this.partialBuffer = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, this.partialBuffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(6), gl.DYNAMIC_DRAW);
    this.segVao = this.ribbonVao(this.segBuffer);
    this.partialVao = this.ribbonVao(this.partialBuffer);
    this.segments = 0;
    // offsetX moves the view's centre right by this many CSS pixels, so a panel over the
    // left of the canvas doesn't cover the middle of the bed.
    this.offsetX = 0;

    this.buildBed();
    this.buildHead();
    this.buildTram();
    this.viewProj = null;
  }

  ribbonVao(buffer) {
    const gl = this.gl;
    const vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    const corner = gl.getAttribLocation(this.ribbon.p, "a_corner");
    gl.bindBuffer(gl.ARRAY_BUFFER, this.corner);
    gl.enableVertexAttribArray(corner);
    gl.vertexAttribPointer(corner, 2, gl.FLOAT, false, 0, 0);
    gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
    for (const name of ["a_from", "a_to"]) {
      const at = gl.getAttribLocation(this.ribbon.p, name);
      gl.enableVertexAttribArray(at);
      gl.vertexAttribDivisor(at, 1);
    }
    gl.bindVertexArray(null);
    return vao;
  }

  // Points the instance attributes at a range of segments. WebGL 2 has no base instance,
  // so the offset is how a draw starts part way along.
  pointSegments(first) {
    const gl = this.gl;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.segBuffer);
    const from = gl.getAttribLocation(this.ribbon.p, "a_from");
    const to = gl.getAttribLocation(this.ribbon.p, "a_to");
    gl.vertexAttribPointer(from, 3, gl.FLOAT, false, 24, first * 24);
    gl.vertexAttribPointer(to, 3, gl.FLOAT, false, 24, first * 24 + 12);
  }

  flatVao(data) {
    const gl = this.gl;
    const vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(data), gl.STATIC_DRAW);
    const at = gl.getAttribLocation(this.flat.p, "a_pos");
    gl.enableVertexAttribArray(at);
    gl.vertexAttribPointer(at, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
    return vao;
  }

  buildBed() {
    const fine = [];
    const coarse = [];
    for (let i = 0; i <= BED; i += 10) {
      const to = i % 50 === 0 ? coarse : fine;
      to.push(i, 0, 0, i, BED, 0, 0, i, 0, BED, i, 0);
    }
    this.fineVao = this.flatVao(fine);
    this.fineCount = fine.length / 3;
    this.coarseVao = this.flatVao(coarse);
    this.coarseCount = coarse.length / 3;
    this.plateVao = this.flatVao([0, 0, -0.05, BED, 0, -0.05, 0, BED, -0.05, BED, BED, -0.05]);
  }

  // A nozzle pointing down, with its tip at the origin.
  buildHead() {
    const tris = [];
    const sides = 16;
    const r = 5;
    const h = 11;
    for (let i = 0; i < sides; i++) {
      const a = (i / sides) * Math.PI * 2;
      const b = ((i + 1) / sides) * Math.PI * 2;
      const ax = Math.cos(a) * r, ay = Math.sin(a) * r, bx = Math.cos(b) * r, by = Math.sin(b) * r;
      tris.push(0, 0, 0, ax, ay, h, bx, by, h);
      tris.push(0, 0, h, bx, by, h, ax, ay, h);
    }
    this.headVao = this.flatVao(tris);
    this.headCount = tris.length / 3;
    this.dropBuffer = this.gl.createBuffer();
    const gl = this.gl;
    this.dropVao = gl.createVertexArray();
    gl.bindVertexArray(this.dropVao);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.dropBuffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(18), gl.DYNAMIC_DRAW);
    const at = gl.getAttribLocation(this.flat.p, "a_pos");
    gl.enableVertexAttribArray(at);
    gl.vertexAttribPointer(at, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
  }

  litVao(data) {
    const gl = this.gl;
    const vao = gl.createVertexArray();
    gl.bindVertexArray(vao);
    gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(data), gl.STATIC_DRAW);
    const at = gl.getAttribLocation(this.lit.p, "a_pos");
    gl.enableVertexAttribArray(at);
    gl.vertexAttribPointer(at, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
    return { vao, count: data.length / 3 };
  }

  // Unit shapes for the tram view: a box standing on the origin, a diamond around it, and
  // a flat ring around it.
  buildTram() {
    const box = [];
    const c = [[-0.5, -0.5], [0.5, -0.5], [0.5, 0.5], [-0.5, 0.5]];
    for (let i = 0; i < 4; i++) {
      const [ax, ay] = c[i];
      const [bx, by] = c[(i + 1) % 4];
      box.push(ax, ay, 0, bx, by, 0, bx, by, 1, ax, ay, 0, bx, by, 1, ax, ay, 1);
    }
    box.push(-0.5, -0.5, 1, 0.5, -0.5, 1, 0.5, 0.5, 1, -0.5, -0.5, 1, 0.5, 0.5, 1, -0.5, 0.5, 1);
    box.push(-0.5, -0.5, 0, 0.5, 0.5, 0, 0.5, -0.5, 0, -0.5, -0.5, 0, -0.5, 0.5, 0, 0.5, 0.5, 0);
    this.boxMesh = this.litVao(box);

    const diamond = [];
    const around = [[1, 0], [0, 1], [-1, 0], [0, -1]];
    for (let i = 0; i < 4; i++) {
      const [ax, ay] = around[i];
      const [bx, by] = around[(i + 1) % 4];
      diamond.push(0, 0, 1, ax, ay, 0, bx, by, 0, 0, 0, -1, bx, by, 0, ax, ay, 0);
    }
    this.diamondMesh = this.litVao(diamond);

    const ring = [];
    const sides = 32;
    for (let i = 0; i < sides; i++) {
      const a = (i / sides) * Math.PI * 2;
      const b = ((i + 1) / sides) * Math.PI * 2;
      const [ca, sa, cb, sb] = [Math.cos(a), Math.sin(a), Math.cos(b), Math.sin(b)];
      ring.push(ca * 0.7, sa * 0.7, 0, ca, sa, 0, cb, sb, 0, ca * 0.7, sa * 0.7, 0, cb, sb, 0, cb * 0.7, sb * 0.7, 0);
    }
    this.ringMesh = this.litVao(ring);

    const gl = this.gl;
    this.tramBuffer = gl.createBuffer();
    this.tramVao = gl.createVertexArray();
    gl.bindVertexArray(this.tramVao);
    gl.bindBuffer(gl.ARRAY_BUFFER, this.tramBuffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array(48), gl.DYNAMIC_DRAW);
    const at = gl.getAttribLocation(this.flat.p, "a_pos");
    gl.enableVertexAttribArray(at);
    gl.vertexAttribPointer(at, 3, gl.FLOAT, false, 0, 0);
    gl.bindVertexArray(null);
  }

  // turn is an angle around Z, in radians.
  shape(mesh, offset, scale, color, turn = 0) {
    const gl = this.gl;
    gl.uniform2f(this.lit.uniforms.u_turn, Math.cos(turn), Math.sin(turn));
    gl.uniform3f(this.lit.uniforms.u_offset, offset[0], offset[1], offset[2]);
    gl.uniform3f(this.lit.uniforms.u_scale, scale[0], scale[1], scale[2]);
    gl.uniform4f(this.lit.uniforms.u_color, color[0], color[1], color[2], color[3]);
    gl.bindVertexArray(mesh.vao);
    gl.drawArrays(gl.TRIANGLES, 0, mesh.count);
  }

  // The tram check: rings at the probe points, a pin for each reading standing up from its
  // point, and a faint plane at the average they're measured from.
  drawTram(viewProj, tram) {
    const gl = this.gl;
    gl.useProgram(this.lit.p);
    gl.uniformMatrix4fv(this.lit.uniforms.u_viewProj, false, viewProj);
    for (const p of tram.points) {
      const r = p.big ? 6 : 4.5;
      this.shape(this.ringMesh, [p.x, p.y, 0.04], [r, r, 1], p.color);
    }
    for (const p of tram.pins) {
      this.shape(this.boxMesh, [p.x, p.y, 0], [0.8, 0.8, p.h], p.color);
      this.shape(this.diamondMesh, [p.x, p.y, p.h], [2.4, 2.4, 2.4], p.color);
    }
    if (tram.plane) {
      const [lo, hi] = tram.plane;
      const z = PIN_BASE;
      gl.useProgram(this.flat.p);
      gl.uniformMatrix4fv(this.flat.uniforms.u_viewProj, false, viewProj);
      gl.bindVertexArray(this.tramVao);
      gl.bindBuffer(gl.ARRAY_BUFFER, this.tramBuffer);
      gl.bufferSubData(gl.ARRAY_BUFFER, 0, new Float32Array([lo, lo, z, hi, lo, z, lo, hi, z, hi, hi, z]));
      gl.enable(gl.BLEND);
      gl.depthMask(false);
      gl.uniform4f(this.flat.uniforms.u_color, 0.62, 0.72, 0.85, 0.07);
      gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
      gl.bufferSubData(gl.ARRAY_BUFFER, 0, new Float32Array([
        lo, lo, z, hi, lo, z, hi, lo, z, hi, hi, z, hi, hi, z, lo, hi, z, lo, hi, z, lo, lo, z,
      ]));
      gl.uniform4f(this.flat.uniforms.u_color, 0.62, 0.72, 0.85, 0.3);
      gl.drawArrays(gl.LINES, 0, 8);
      gl.depthMask(true);
      gl.disable(gl.BLEND);
    }
  }

  // The mesh's heights as a surface, from data that's x, y, z, r, g, b for each corner of
  // each triangle. It's only sent to the GPU when it changes.
  drawSurface(viewProj, data) {
    const gl = this.gl;
    if (data !== this.surfData) {
      this.surfData = data;
      if (!this.surfVao) {
        this.surfVao = gl.createVertexArray();
        this.surfBuffer = gl.createBuffer();
        gl.bindVertexArray(this.surfVao);
        gl.bindBuffer(gl.ARRAY_BUFFER, this.surfBuffer);
        const pos = gl.getAttribLocation(this.surf.p, "a_pos");
        const col = gl.getAttribLocation(this.surf.p, "a_color");
        gl.enableVertexAttribArray(pos);
        gl.vertexAttribPointer(pos, 3, gl.FLOAT, false, 24, 0);
        gl.enableVertexAttribArray(col);
        gl.vertexAttribPointer(col, 3, gl.FLOAT, false, 24, 12);
      }
      gl.bindBuffer(gl.ARRAY_BUFFER, this.surfBuffer);
      gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
    }
    gl.useProgram(this.surf.p);
    gl.uniformMatrix4fv(this.surf.uniforms.u_viewProj, false, viewProj);
    gl.uniform1f(this.surf.uniforms.u_alpha, 0.88);
    gl.bindVertexArray(this.surfVao);
    gl.enable(gl.BLEND);
    gl.depthMask(false);
    gl.drawArrays(gl.TRIANGLES, 0, data.length / 6);
    gl.depthMask(true);
    gl.disable(gl.BLEND);
  }

  // The probe beside the nozzle, joined to it, with its pin let down by deploy (0 to 1).
  drawProbe(viewProj, p, deploy) {
    const gl = this.gl;
    gl.useProgram(this.lit.p);
    gl.uniformMatrix4fv(this.lit.uniforms.u_viewProj, false, viewProj);
    const x = p[0] + PROBE_OFFSET[0];
    const y = p[1] + PROBE_OFFSET[1];
    const body = [0.55, 0.6, 0.68, 1];
    // The pin's tip is 3 mm above the nozzle's tip when up, and touches the bed with the
    // nozzle at TOUCH_Z when down.
    const tip = p[2] + 3 - (3 + TOUCH_Z) * deploy;
    this.shape(this.boxMesh, [x, y, p[2] + 3], [6, 6, 10], body);
    this.shape(this.boxMesh, [x, y, tip], [1, 1, p[2] + 3 - tip + 0.01], [0.93, 0.95, 0.98, 1]);
    // A bar from the probe to the nozzle's mount, so they read as one head.
    const len = Math.hypot(p[0] - x, p[1] - y);
    const turn = Math.atan2(p[1] - y, p[0] - x);
    this.shape(this.boxMesh, [(p[0] + x) / 2, (p[1] + y) / 2, p[2] + 11], [len + 6, 2.5, 2], body, turn);
  }

  // project gives where a point is drawn on the canvas, in CSS pixels, or null if it's
  // behind the camera.
  project(p) {
    const m = this.viewProj;
    if (!m) return null;
    const x = m[0] * p[0] + m[4] * p[1] + m[8] * p[2] + m[12];
    const y = m[1] * p[0] + m[5] * p[1] + m[9] * p[2] + m[13];
    const w = m[3] * p[0] + m[7] * p[1] + m[11] * p[2] + m[15];
    if (w <= 0) return null;
    return [((x / w + 1) / 2) * this.canvas.clientWidth, ((1 - y / w) / 2) * this.canvas.clientHeight];
  }

  setModel(model) {
    const gl = this.gl;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.segBuffer);
    gl.bufferData(gl.ARRAY_BUFFER, model ? model.segments : new Float32Array(0), gl.STATIC_DRAW);
    this.segments = model ? model.segmentCount : 0;
  }

  frame(model) {
    if (!model) return;
    this.home = {
      target: [model.center[0], model.center[1], model.center[2]],
      distance: Math.max(model.radius * 3.2, 120),
      yaw: -0.65,
      pitch: 0.6,
    };
    this.reset();
  }

  // A tall screen shows less from side to side, so it stands further back to fit.
  reset() {
    this.camera = { ...this.home, target: [...this.home.target] };
    const aspect = this.canvas.clientWidth / Math.max(this.canvas.clientHeight, 1);
    if (aspect > 0 && aspect < 0.7) this.camera.distance *= 0.7 / aspect;
    this.moved = false;
  }

  eye() {
    const c = this.camera;
    return [
      c.target[0] + c.distance * Math.cos(c.pitch) * Math.sin(c.yaw),
      c.target[1] - c.distance * Math.cos(c.pitch) * Math.cos(c.yaw),
      c.target[2] + c.distance * Math.sin(c.pitch),
    ];
  }

  // Moves the target across the screen, by a drag in pixels.
  pan(dx, dy) {
    const c = this.camera;
    const world = (2 * c.distance * Math.tan(this.fov() / 2)) / this.canvas.clientHeight;
    const forward = unit(sub(c.target, this.eye()));
    const right = unit(cross(forward, [0, 0, 1]));
    const up = cross(right, forward);
    for (let i = 0; i < 3; i++) c.target[i] += (-dx * right[i] + dy * up[i]) * world;
  }

  fov() { return 0.7; }

  resize() {
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = Math.round(this.canvas.clientWidth * dpr);
    const h = Math.round(this.canvas.clientHeight * dpr);
    if (this.canvas.width !== w || this.canvas.height !== h) {
      this.canvas.width = w;
      this.canvas.height = h;
    }
    this.gl.viewport(0, 0, w, h);
  }

  draw(view) {
    const gl = this.gl;
    this.resize();
    gl.clear(gl.COLOR_BUFFER_BIT | gl.DEPTH_BUFFER_BIT);
    const eye = this.eye();
    const aspect = this.canvas.width / Math.max(this.canvas.height, 1);
    const proj = perspective(this.fov(), aspect, Math.max(this.camera.distance * 0.01, 0.5), this.camera.distance * 10 + 1000);
    // A shift of the image on screen, which keeps the perspective of a centred camera.
    if (this.offsetX) proj[8] -= (2 * this.offsetX) / Math.max(this.canvas.clientWidth, 1);
    const viewProj = multiply(proj, lookAt(eye, this.camera.target, [0, 0, 1]));
    this.viewProj = viewProj;

    // The bed: plate first, then its lines.
    gl.useProgram(this.flat.p);
    gl.uniformMatrix4fv(this.flat.uniforms.u_viewProj, false, viewProj);
    gl.uniform4f(this.flat.uniforms.u_color, 0.13, 0.15, 0.18, 1);
    gl.bindVertexArray(this.plateVao);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    gl.uniform4f(this.flat.uniforms.u_color, 0.19, 0.21, 0.25, 1);
    gl.bindVertexArray(this.fineVao);
    gl.drawArrays(gl.LINES, 0, this.fineCount);
    gl.uniform4f(this.flat.uniforms.u_color, 0.29, 0.32, 0.37, 1);
    gl.bindVertexArray(this.coarseVao);
    gl.drawArrays(gl.LINES, 0, this.coarseCount);

    const pixel = (2 * Math.tan(this.fov() / 2)) / Math.max(this.canvas.height, 1);
    const useRibbon = (r, g, b, a) => {
      gl.useProgram(this.ribbon.p);
      gl.uniformMatrix4fv(this.ribbon.uniforms.u_viewProj, false, viewProj);
      gl.uniform3f(this.ribbon.uniforms.u_eye, eye[0], eye[1], eye[2]);
      gl.uniform1f(this.ribbon.uniforms.u_width, FILAMENT_WIDTH);
      gl.uniform1f(this.ribbon.uniforms.u_pixel, pixel);
      gl.uniform4f(this.ribbon.uniforms.u_color, r, g, b, a);
    };

    const done = view.head ? view.head.done : view.all ? this.segments : 0;

    // What's printed so far.
    if (done > 0) {
      useRibbon(0.96, 0.56, 0.25, 1);
      gl.bindVertexArray(this.segVao);
      this.pointSegments(0);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, done);
    }
    if (view.head && view.head.partial) {
      useRibbon(0.96, 0.56, 0.25, 1);
      gl.bindBuffer(gl.ARRAY_BUFFER, this.partialBuffer);
      gl.bufferSubData(gl.ARRAY_BUFFER, 0, new Float32Array(view.head.partial));
      gl.bindVertexArray(this.partialVao);
      const from = gl.getAttribLocation(this.ribbon.p, "a_from");
      const to = gl.getAttribLocation(this.ribbon.p, "a_to");
      gl.vertexAttribPointer(from, 3, gl.FLOAT, false, 24, 0);
      gl.vertexAttribPointer(to, 3, gl.FLOAT, false, 24, 12);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, 1);
    }

    // What's still to come, as a ghost that doesn't hide anything.
    if (done < this.segments) {
      gl.enable(gl.BLEND);
      gl.depthMask(false);
      useRibbon(0.62, 0.72, 0.85, view.head ? 0.09 : 0.16);
      gl.bindVertexArray(this.segVao);
      this.pointSegments(done);
      gl.drawArraysInstanced(gl.TRIANGLE_STRIP, 0, 4, this.segments - done);
      gl.depthMask(true);
      gl.disable(gl.BLEND);
    }

    if (view.tram) {
      this.drawTram(viewProj, view.tram);
      if (view.tram.surface) this.drawSurface(viewProj, view.tram.surface);
      if (view.tram.nozzle) {
        this.drawProbe(viewProj, view.tram.nozzle, view.tram.deploy);
        this.drawHead(viewProj, view.tram.nozzle);
      }
    } else if (view.head && view.showHead) {
      this.drawHead(viewProj, view.head.pos);
    }
    gl.bindVertexArray(null);
  }

  drawHead(viewProj, p) {
    const gl = this.gl;
    gl.useProgram(this.flat.p);
    gl.uniformMatrix4fv(this.flat.uniforms.u_viewProj, false, viewProj);

    // A line down to the bed, and a cross where it lands, to show where it is in depth.
    const arm = 6;
    gl.bindBuffer(gl.ARRAY_BUFFER, this.dropBuffer);
    gl.bufferSubData(gl.ARRAY_BUFFER, 0, new Float32Array([
      p[0], p[1], p[2], p[0], p[1], 0,
      p[0] - arm, p[1], 0.02, p[0] + arm, p[1], 0.02,
      p[0], p[1] - arm, 0.02, p[0], p[1] + arm, 0.02,
    ]));
    gl.uniform4f(this.flat.uniforms.u_color, 0.8, 0.82, 0.85, 1);
    gl.bindVertexArray(this.dropVao);
    gl.drawArrays(gl.LINES, 0, 6);

    // The nozzle, drawn on top so the print can't hide it.
    const moved = multiply(viewProj, new Float32Array([1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, p[0], p[1], p[2], 1]));
    gl.uniformMatrix4fv(this.flat.uniforms.u_viewProj, false, moved);
    gl.uniform4f(this.flat.uniforms.u_color, 0.93, 0.95, 0.98, 1);
    gl.bindVertexArray(this.headVao);
    gl.disable(gl.DEPTH_TEST);
    gl.drawArrays(gl.TRIANGLES, 0, this.headCount);
    gl.enable(gl.DEPTH_TEST);
  }
}

// ---- The head during a tram check ----
//
// The server says which phase a check is in (homing, probing a corner, parking), where it's
// going, and how long the phase has run. Nothing reports the head's position, so it's drawn
// moving the way the firmware moves it, at this printer's speeds, and it waits at the end
// of a phase's moves until the server says the next has begun. The figures are from M851,
// C851, Park in internal/marlin/motion.go, and the step times measured on the printer:
// G28 14 to 20 s, each G30 4.8 to 5.4 s, parking 5 s. See notes/probe-view.md.

// M851: the probe is 41.5 mm left of the nozzle and 7 mm in front. G30 X Y puts the probe
// over the point, then moves the nozzle there before answering, as seen on the printer.
const PROBE_OFFSET = [-41.5, -7];
// The nozzle's height between probes, as the LCD showed (5.9), and when the probe touches
// the bed (M851's Z of -0.9 means the nozzle is 0.9 mm up then).
const TRAVEL_Z = 6;
const TOUCH_Z = 0.9;
// mm/s. Probe travel is set to 200, but with acceleration a corner-to-corner move averages
// less. Probing down is C851's S480, and homing X and Y is 50.
const TRAVEL_SPEED = 120;
const PROBE_SPEED = 8;
const LIFT_SPEED = 10;
const HOME_SPEED = 50;
// G28 ends with the probe over the middle of the bed, so the nozzle at X156.5 Y122, the
// position reported before G29 on the printer.
const HOMED = [115 - PROBE_OFFSET[0], 115 - PROBE_OFFSET[1], 10];
// Where the head is assumed to start if nothing has been drawn yet: parked.
const PARKED = [115, 115, 30];
// Readings are drawn as pins standing up from the probe points, with the average at
// PIN_BASE mm and each 0.05 mm away from it as PIN_SCALE * 0.05 = 20 mm.
const PIN_BASE = 30;
const PIN_SCALE = 400;
// How quickly the drawn head catches up with where the script puts it, in seconds.
const FOLLOW_SECONDS = 0.12;
const TRAM_COLORS = {
  pending: [0.45, 0.5, 0.58, 1],
  current: [0.94, 0.53, 0.24, 1],
  good: [0.34, 0.77, 0.48, 1],
  near: [0.95, 0.75, 0.3, 1],
  bad: [0.95, 0.39, 0.35, 1],
};

const lerp3 = (a, b, f) => [a[0] + (b[0] - a[0]) * f, a[1] + (b[1] - a[1]) * f, a[2] + (b[2] - a[2]) * f];
const ease = (f) => f * f * (3 - 2 * f);

// A phase's moves from a starting point, as a list of timed steps.
class TramScript {
  constructor(from) {
    this.steps = [];
    this.at = from;
    this.deploy = 0;
  }

  move(to, speed, extra = 0) {
    const d = Math.hypot(to[0] - this.at[0], to[1] - this.at[1], to[2] - this.at[2]);
    if (d < 1e-3 && !extra) return this;
    this.steps.push({ from: this.at, to, deployFrom: this.deploy, deploy: this.deploy, dur: d / speed + extra });
    this.at = to;
    return this;
  }

  // A move that takes dur seconds, whatever its length.
  moveFor(to, dur) {
    if (dur <= 0) return this;
    this.steps.push({ from: this.at, to, deployFrom: this.deploy, deploy: this.deploy, dur });
    this.at = to;
    return this;
  }

  // The probe's pin going down (1) or up (0).
  pin(deploy, dur) {
    this.steps.push({ from: this.at, to: this.at, deployFrom: this.deploy, deploy, dur });
    this.deploy = deploy;
    return this;
  }

  wait(dur) { return this.pin(this.deploy, dur); }

  get seconds() { return this.steps.reduce((sum, s) => sum + s.dur, 0); }

  // Where the nozzle is, and how far the pin is down, a time into the phase.
  pose(t) {
    for (const s of this.steps) {
      if (t < s.dur) {
        const f = ease(Math.max(t, 0) / s.dur);
        return { nozzle: lerp3(s.from, s.to, f), deploy: s.deployFrom + (s.deploy - s.deployFrom) * f };
      }
      t -= s.dur;
    }
    return { nozzle: this.at, deploy: this.deploy };
  }

  static homing(from) {
    const s = new TramScript(from);
    if (from[2] < HOMED[2]) s.move([from[0], from[1], HOMED[2]], LIFT_SPEED);
    s.move([0, s.at[1], s.at[2]], HOME_SPEED).wait(0.6);
    s.move([0, 0, s.at[2]], HOME_SPEED).wait(0.6);
    s.move([HOMED[0], HOMED[1], s.at[2]], TRAVEL_SPEED, 0.2);
    // Z is homed with the probe: fast down, up a little, then slowly down again.
    s.pin(1, 0.4).move([HOMED[0], HOMED[1], TOUCH_Z], PROBE_SPEED * 2).wait(0.2);
    s.move([HOMED[0], HOMED[1], TOUCH_Z + 3], LIFT_SPEED).move([HOMED[0], HOMED[1], TOUCH_Z], PROBE_SPEED / 2);
    return s.pin(0, 0.4).move(HOMED, LIFT_SPEED);
  }

  static probing(from, x, y) {
    const s = new TramScript(from);
    s.move([x - PROBE_OFFSET[0], y - PROBE_OFFSET[1], TRAVEL_Z], TRAVEL_SPEED, 0.15);
    s.pin(1, 0.4).move([s.at[0], s.at[1], TOUCH_Z], PROBE_SPEED).wait(0.2);
    s.move([s.at[0], s.at[1], TRAVEL_Z], LIFT_SPEED).pin(0, 0.4);
    // The move G30 makes after its reading, putting the nozzle where the probe was.
    return s.move([x, y, TRAVEL_Z], TRAVEL_SPEED, 0.15);
  }

  // Park: G0 Z30 F600, then G0 X115 Y115 F3000.
  static parking(from, target) {
    const s = new TramScript(from);
    s.move([from[0], from[1], Math.max(from[2], target.z)], LIFT_SPEED);
    return s.move([target.x, target.y, s.at[2]], 50);
  }
}

// TramScene follows a tram job from the status, and gives what to draw at each frame.
class TramScene {
  constructor() {
    this.job = null;
    this.receivedAt = 0;
    this.key = "";
    this.script = null;
    this.scriptStart = 0;
    this.nozzle = null;
    this.deploy = 0;
    this.heights = [null, null, null, null];
  }

  get active() { return this.job !== null; }

  // update takes the tram job to show, or null to show none.
  update(job) {
    if ((job || null) === this.job) return;
    this.job = job || null;
    this.receivedAt = performance.now();
    if (!job) {
      this.key = "";
      this.script = null;
      this.nozzle = null;
      this.heights = [null, null, null, null];
    }
  }

  // The round whose readings are shown: the one going, or the last one with any.
  shownRound() {
    const rounds = (this.job && this.job.rounds) || [];
    for (let i = rounds.length - 1; i >= 0; i--) if (rounds[i].readings && rounds[i].readings.length) return rounds[i];
    return null;
  }

  lastReading() {
    const rounds = (this.job && this.job.rounds) || [];
    for (let i = rounds.length - 1; i >= 0; i--) {
      const r = rounds[i].readings || [];
      if (r.length) return r[r.length - 1];
    }
    return null;
  }

  // Builds the script for the phase the job is in, once per phase, from where the head
  // should be by then. Only homing and a finished park start from the drawn head.
  scriptFor(now) {
    const job = this.job;
    const rounds = job.rounds || [];
    const readings = rounds.length ? (rounds[rounds.length - 1].readings || []).length : 0;
    const phase = job.state === "running" ? job.phase || "" : job.parked ? "parked" : "stopped";
    const target = job.target || {};
    const key = `${job.id}|${phase}|${target.corner || ""}|${rounds.length}|${readings}`;
    if (key === this.key) return;
    this.key = key;
    const drawn = this.nozzle || PARKED;
    const last = this.lastReading();
    const afterLast = last ? [last.x, last.y, TRAVEL_Z] : HOMED;
    // A phase seen part way through started that long ago. A finished park, which a poll
    // may have missed, is drawn from now.
    const ago = job.state === "running" ? (job.step_seconds || 0) : 0;
    this.scriptStart = this.receivedAt - ago * 1000;
    switch (phase) {
      case "homing": this.script = TramScript.homing(drawn); break;
      case "probing": this.script = TramScript.probing(afterLast, target.x, target.y); break;
      case "parking": this.script = TramScript.parking(afterLast, target); break;
      case "parked": this.script = TramScript.parking(drawn, { x: PARKED[0], y: PARKED[1], z: PARKED[2] }); this.scriptStart = now; break;
      default: this.script = new TramScript(drawn);
    }
  }

  // frame moves the drawn head on by dt seconds and gives the scene, or null.
  frame(now, dt, tolerance) {
    const job = this.job;
    if (!job) return null;
    this.scriptFor(now);
    const pose = this.script.pose((now - this.scriptStart) / 1000);
    if (!this.nozzle) {
      this.nozzle = pose.nozzle;
      this.deploy = pose.deploy;
    } else {
      const f = 1 - Math.exp(-dt / FOLLOW_SECONDS);
      this.nozzle = lerp3(this.nozzle, pose.nozzle, f);
      this.deploy += (pose.deploy - this.deploy) * f;
    }

    // The probe points in the wizard's order, front-left, front-right, back-right, back-left.
    const lo = job.inset ?? 25;
    const hi = 230 - lo;
    const corners = [[lo, lo], [hi, lo], [hi, hi], [lo, hi]];
    const names = ["front-left", "front-right", "back-right", "back-left"];
    const round = this.shownRound();
    const z = [null, null, null, null];
    if (round) for (const r of round.readings) z[names.indexOf(r.corner)] = r.z;
    // Each against the average: the round's own figures once it's done, and the average of
    // what's in so far until then.
    let rel = [null, null, null, null];
    if (round && round.result) {
      rel = round.result.relative_mm;
    } else {
      const have = z.filter((v) => v !== null);
      const avg = have.reduce((a, b) => a + b, 0) / Math.max(have.length, 1);
      rel = z.map((v) => (v === null ? null : v - avg));
    }
    const result = job.state === "running" ? null : job.result;
    const bad = result && !result.pass ? result.corner : null;
    const probing = job.state === "running" && job.phase === "probing" ? (job.target || {}).corner : null;

    const points = [];
    const pins = [];
    let moving = false;
    for (let i = 0; i < 4; i++) {
      const [x, y] = corners[i];
      const r = rel[i];
      let color = TRAM_COLORS.pending;
      if (r !== null) {
        const d = Math.abs(r) / (tolerance / 2);
        color = d > 1 ? TRAM_COLORS.bad : d > 0.5 ? TRAM_COLORS.near : TRAM_COLORS.good;
      }
      if (names[i] === probing) color = TRAM_COLORS.current;
      if (names[i] === bad) color = TRAM_COLORS.bad;
      points.push({ x, y, color, big: names[i] === probing || names[i] === bad });
      if (r === null) {
        this.heights[i] = null;
        continue;
      }
      const want = Math.min(Math.max(PIN_BASE + r * PIN_SCALE, 2), PIN_BASE * 2.5);
      const from = this.heights[i] ?? 0;
      const h = from + (want - from) * (1 - Math.exp(-dt / 0.25));
      this.heights[i] = Math.abs(want - h) < 0.01 ? want : h;
      if (this.heights[i] !== want) moving = true;
      let label = `${r >= 0 ? "+" : "−"}${Math.abs(r).toFixed(3)}`;
      if (names[i] === bad) label += ` · ${result.action}`;
      pins.push({ x, y, h: this.heights[i], color, label, bad: names[i] === bad });
    }
    const still = Math.hypot(...sub(this.nozzle, pose.nozzle)) < 0.01 && Math.abs(this.deploy - pose.deploy) < 0.01 &&
      (now - this.scriptStart) / 1000 > this.script.seconds;
    return {
      points,
      pins,
      plane: pins.length > 0 ? [lo, hi] : null,
      nozzle: this.nozzle,
      deploy: this.deploy,
      // Whether anything is still changing, so the view knows to keep drawing.
      moving: moving || !still,
    };
  }
}

// ---- The mesh ----

// G29 P1 in the file's start probes a 5 by 5 mesh from 25 to 205 mm (C29). The firmware
// only says "Probing mesh point N/25." as each point starts, before the head moves there,
// so which point N is, and where the head is, are worked out by copying UBL's order, and
// drawn as an estimate. The order and timing are internal/sim/mesh.go's, which tests them
// against a real G29, so keep the two in step. No heights are reported.
const MESH_LO = 25;
const MESH_HI = 205;
// Fitted to the real G29: about 1.66 s probing each point, after a travel as long as a
// move of the longer axis at up to 180 mm/s with 460 mm/s² acceleration.
const MESH_TRAVEL_SPEED = 180;
const MESH_TRAVEL_ACCEL = 460;
// The probing itself, adding up to 1.66 s: the pin down, down to the bed at C851's 8 mm/s,
// a pause, back up, the pin up.
const MESH_PIN_DOWN = 0.25;
const MESH_TOUCH_WAIT = 0.1;
const MESH_PIN_UP = 0.16;
const MESH_COLORS = {
  pending: TRAM_COLORS.pending,
  current: TRAM_COLORS.current,
  // Probed, height unknown, so not the green that means good.
  done: [0.38, 0.62, 0.92, 1],
};

function meshTravelSeconds(d) {
  if (d <= 0) return 0;
  if (d < (MESH_TRAVEL_SPEED * MESH_TRAVEL_SPEED) / MESH_TRAVEL_ACCEL) return 2 * Math.sqrt(d / MESH_TRAVEL_ACCEL);
  return d / MESH_TRAVEL_SPEED + MESH_TRAVEL_SPEED / MESH_TRAVEL_ACCEL;
}

// meshOrder copies UBL's rule: the next point is the unprobed one with the least distance
// from where the probe was before G29, plus a tenth of its distance from the nozzle now.
// Ties go to the first in X then Y. Each point has where the probe touches, where the
// nozzle is then, and how long its travel there takes.
function meshOrder(size) {
  const step = (MESH_HI - MESH_LO) / (size - 1);
  const ref = [HOMED[0] + PROBE_OFFSET[0], HOMED[1] + PROBE_OFFSET[1]];
  let nozzle = [HOMED[0], HOMED[1]];
  const done = new Set();
  const out = [];
  for (let n = 0; n < size * size; n++) {
    let best = -1;
    let bestD = Infinity;
    for (let i = 0; i < size; i++) {
      for (let j = 0; j < size; j++) {
        if (done.has(i * size + j)) continue;
        const x = MESH_LO + i * step;
        const y = MESH_LO + j * step;
        const d = Math.hypot(ref[0] - x, ref[1] - y) + 0.1 * Math.hypot(nozzle[0] - x, nozzle[1] - y);
        if (d < bestD) {
          bestD = d;
          best = i * size + j;
        }
      }
    }
    done.add(best);
    const x = MESH_LO + Math.floor(best / size) * step;
    const y = MESH_LO + (best % size) * step;
    const next = [x - PROBE_OFFSET[0], y - PROBE_OFFSET[1]];
    const travel = meshTravelSeconds(Math.max(Math.abs(next[0] - nozzle[0]), Math.abs(next[1] - nozzle[1])));
    out.push({ x, y, nozzle: next, travel });
    nozzle = next;
  }
  return out;
}

// The head's moves for one point, from the last one's nozzle position.
function meshScript(order, index) {
  const p = order[index];
  const from = index > 0 ? [...order[index - 1].nozzle, TRAVEL_Z] : [HOMED[0], HOMED[1], TRAVEL_Z];
  const s = new TramScript(from);
  s.moveFor([p.nozzle[0], p.nozzle[1], TRAVEL_Z], p.travel);
  s.pin(1, MESH_PIN_DOWN).move([p.nozzle[0], p.nozzle[1], TOUCH_Z], PROBE_SPEED).wait(MESH_TOUCH_WAIT);
  return s.move([p.nozzle[0], p.nozzle[1], TRAVEL_Z], LIFT_SPEED).pin(0, MESH_PIN_UP);
}

// MeshScene follows a print job's mesh from the status, and gives what to draw at each
// frame, in the same shape as TramScene's.
class MeshScene {
  constructor() {
    this.job = null;
    this.receivedAt = 0;
    this.orders = new Map();
    this.nozzle = null;
    this.deploy = 0;
  }

  get active() { return this.job !== null; }

  // update takes the print job whose mesh to show, or null to show none.
  update(job) {
    if ((job || null) === this.job) return;
    this.job = job && job.mesh ? job : null;
    this.receivedAt = performance.now();
    if (!this.job) this.nozzle = null;
  }

  order(size) {
    if (!this.orders.has(size)) this.orders.set(size, meshOrder(size));
    return this.orders.get(size);
  }

  frame(now, dt) {
    const mesh = this.job && this.job.mesh;
    if (!mesh) return null;
    const size = Math.round(Math.sqrt(mesh.of));
    if (size < 2 || size * size !== mesh.of) return null;
    const order = this.order(size);
    // The point the server last heard of, how long ago it started, and then on by the
    // estimate to the next if that's due, as status only comes every 2 seconds. Never
    // more than one ahead, so a G29 that's stopped doesn't run on.
    let index = mesh.point - 1;
    let t = (mesh.point_seconds || 0) + (now - this.receivedAt) / 1000;
    let script = meshScript(order, index);
    if (t > script.seconds && index + 1 < order.length) {
      t -= script.seconds;
      index++;
      script = meshScript(order, index);
    }
    const pose = script.pose(t);
    if (!this.nozzle) {
      this.nozzle = pose.nozzle;
      this.deploy = pose.deploy;
    } else {
      const f = 1 - Math.exp(-dt / FOLLOW_SECONDS);
      this.nozzle = lerp3(this.nozzle, pose.nozzle, f);
      this.deploy += (pose.deploy - this.deploy) * f;
    }
    const finished = index === order.length - 1 && t > script.seconds;
    const points = order.map((p, k) => {
      const current = k === index && !finished;
      const color = current ? MESH_COLORS.current : k <= index ? MESH_COLORS.done : MESH_COLORS.pending;
      return { x: p.x, y: p.y, color, big: current };
    });
    const at = order[index];
    const labels = finished ? [] : [{ x: at.x, y: at.y, h: 22, label: `point ${index + 1} of ${order.length}, estimated` }];
    const still = Math.hypot(...sub(this.nozzle, pose.nozzle)) < 0.01 && Math.abs(this.deploy - pose.deploy) < 0.01;
    return { points, pins: [], labels, plane: null, nozzle: this.nozzle, deploy: this.deploy, moving: !finished || !still };
  }
}

// ---- Mesh heights ----

// The heights stand up from the same base as the tram pins, scaled the same, measured from
// their average. Colours go from blue below to orange above, full at HEIGHT_FULL_MM, a
// fixed scale so two meshes can be compared by eye.
const HEIGHT_FULL_MM = 0.1;
const HEIGHT_STEPS = 6;
const HEIGHT_LOW = [0.3, 0.55, 0.95];
const HEIGHT_MID = [0.8, 0.83, 0.87];
const HEIGHT_HIGH = [0.96, 0.5, 0.25];

function heightColor(d) {
  const f = Math.max(-1, Math.min(1, d / HEIGHT_FULL_MM));
  return f < 0 ? lerp3(HEIGHT_MID, HEIGHT_LOW, -f) : lerp3(HEIGHT_MID, HEIGHT_HIGH, f);
}

// HeightsScene draws a mesh read from the firmware (marlin.MeshReport: z[row][column], row
// 0 at the front, null where there's no height) as a surface over the bed, with a pin and
// label at each point. Between points it blends as UBL does, linearly in X and Y.
class HeightsScene {
  constructor() {
    this.view = null;
  }

  update(report) {
    this.view = report ? this.build(report) : null;
  }

  build(report) {
    const z = report.z || [];
    const rows = z.length;
    const cols = rows ? z[0].length : 0;
    if (rows < 2 || cols < 2) return null;
    const known = z.flat().filter((v) => v != null);
    if (!known.length) return null;
    const mean = known.reduce((a, b) => a + b, 0) / known.length;
    const lift = (v) => Math.min(Math.max(PIN_BASE + (v - mean) * PIN_SCALE, 2), PIN_BASE * 2.5);
    const at = (i) => MESH_LO + ((MESH_HI - MESH_LO) * i) / (cols - 1);
    const atY = (j) => MESH_LO + ((MESH_HI - MESH_LO) * j) / (rows - 1);

    const data = [];
    const corner = (x, y, v) => {
      const c = heightColor(v - mean);
      data.push(x, y, lift(v), c[0], c[1], c[2]);
    };
    for (let j = 0; j + 1 < rows; j++) {
      for (let i = 0; i + 1 < cols; i++) {
        const a = z[j][i], b = z[j][i + 1], c = z[j + 1][i], d = z[j + 1][i + 1];
        if (a == null || b == null || c == null || d == null) continue;
        const v = (u, w) => a * (1 - u) * (1 - w) + b * u * (1 - w) + c * (1 - u) * w + d * u * w;
        for (let sj = 0; sj < HEIGHT_STEPS; sj++) {
          for (let si = 0; si < HEIGHT_STEPS; si++) {
            const u0 = si / HEIGHT_STEPS, u1 = (si + 1) / HEIGHT_STEPS;
            const w0 = sj / HEIGHT_STEPS, w1 = (sj + 1) / HEIGHT_STEPS;
            const x0 = at(i) + (at(i + 1) - at(i)) * u0, x1 = at(i) + (at(i + 1) - at(i)) * u1;
            const y0 = atY(j) + (atY(j + 1) - atY(j)) * w0, y1 = atY(j) + (atY(j + 1) - atY(j)) * w1;
            corner(x0, y0, v(u0, w0)); corner(x1, y0, v(u1, w0)); corner(x1, y1, v(u1, w1));
            corner(x0, y0, v(u0, w0)); corner(x1, y1, v(u1, w1)); corner(x0, y1, v(u0, w1));
          }
        }
      }
    }

    const points = [], pins = [], labels = [];
    for (let j = 0; j < rows; j++) {
      for (let i = 0; i < cols; i++) {
        const v = z[j][i];
        const x = at(i), y = atY(j);
        if (v == null) {
          points.push({ x, y, color: MESH_COLORS.pending, big: false });
          continue;
        }
        const c = heightColor(v - mean);
        points.push({ x, y, color: [...c, 1], big: false });
        pins.push({ x, y, h: lift(v), color: [...c, 1] });
        labels.push({ x, y, h: lift(v), label: (v >= 0 ? "+" : "") + v.toFixed(3) });
      }
    }
    return {
      points, pins, labels, plane: [MESH_LO, MESH_HI], surface: new Float32Array(data),
      nozzle: null, deploy: 0, moving: false,
    };
  }

  frame() {
    return this.view;
  }
}

// ---- Following a job ----

// PrintView draws the job's file on a canvas, with the head where the job's position puts
// it, and lets the view be turned, moved and zoomed. Give it each job from the status.
class PrintView {
  // modelURL gives the address of a job's moves, from its ID. labels, if given, is an
  // element over the canvas for the tram view's readings.
  constructor(canvas, modelURL, labels) {
    this.canvas = canvas;
    this.modelURL = modelURL;
    this.labels = labels || null;
    this.tram = new TramScene();
    this.mesh = new MeshScene();
    this.heights = new HeightsScene();
    this.tolerance = 0.05;
    this.tramDrawn = false;
    this.job = null;
    this.model = null;
    this.modelJob = null;
    this.modelMissing = false;
    this.loading = false;
    // Where the drawn head is, in seconds along the file.
    this.playhead = 0;
    this.target = 0;
    this.coast = 0;
    this.updatedAt = 0;
    this.lastFrame = 0;
    this.lastDraw = 0;
    this.dirty = true;
    this.renderer = null;
    try {
      this.renderer = new Renderer(canvas);
    } catch (err) {
      console.error(err);
      return;
    }
    this.listen();
    requestAnimationFrame((now) => this.tick(now));
  }

  get ok() { return this.renderer !== null; }

  // update takes the latest job, or null, from a status poll.
  update(job) {
    this.job = job || null;
    if (job && job.id !== this.modelJob) this.loadModel(job);
    this.updateTarget(job);
    this.dirty = true;
  }

  // updateTram takes the tram job to draw over the bed, or null for none, and the limit
  // its readings are coloured against.
  updateTram(job, tolerance) {
    this.tram.update(job);
    if (tolerance) this.tolerance = tolerance;
    this.dirty = true;
  }

  // updateMesh takes the print job whose mesh probing to draw, or null for none. A tram
  // check's view comes first, if both are given.
  updateMesh(job) {
    this.mesh.update(job);
    this.dirty = true;
  }

  // updateHeights takes a mesh read from the firmware to draw as a surface, or null for
  // none. A tram check or a mesh being probed comes first.
  updateHeights(report) {
    this.heights.update(report);
    this.dirty = true;
  }

  // While the tram view shows, the camera looks at the whole bed, unless it's been moved,
  // and afterwards it goes back to the print.
  frameFor(tram) {
    const r = this.renderer;
    if (tram === this.tramDrawn) return;
    this.tramDrawn = tram;
    if (r.moved) return;
    if (tram) {
      r.home = { target: [BED / 2, BED / 2, 15], distance: 400, yaw: -0.55, pitch: 0.62 };
      r.reset();
    } else if (this.model) {
      r.frame(this.model);
    } else {
      r.home = { target: [BED / 2, BED / 2, 0], distance: 380, yaw: -0.65, pitch: 0.75 };
      r.reset();
    }
  }

  placeLabels(tram) {
    const box = this.labels;
    if (!box) return;
    const pins = tram ? tram.labels || tram.pins : [];
    while (box.children.length < pins.length) box.append(document.createElement("span"));
    while (box.children.length > pins.length) box.lastChild.remove();
    const rect = this.canvas.getBoundingClientRect();
    pins.forEach((p, i) => {
      const el = box.children[i];
      const at = this.renderer.project([p.x, p.y, p.h + 3]);
      el.hidden = !at || at[0] < 0 || at[1] < 0 || at[0] > rect.width || at[1] > rect.height;
      if (!at) return;
      if (el.textContent !== p.label) el.textContent = p.label;
      el.className = p.bad ? "bad" : "";
      el.style.transform = `translate(${(rect.left + at[0]).toFixed(1)}px, ${(rect.top + at[1]).toFixed(1)}px) translate(-50%, -100%)`;
    });
  }

  // details are the layer and time left at the drawn head.
  details() {
    const job = this.job;
    const model = this.model;
    if (!job || !model) return { layer: "–", left: "–" };
    const at = model.at(this.playhead);
    return {
      layer: at ? `${job.state === "finished" ? model.layers : at.layer} of ${model.layers}` : "–",
      left: printing(job) ? `about ${clock(model.seconds - this.playhead)}` : "–",
    };
  }

  // Sets where the head should be from the job's position.
  updateTarget(job) {
    const model = this.model;
    if (!model || !job) return;
    if (job.state === "finished") {
      this.target = model.seconds;
      this.playhead = model.seconds;
      return;
    }
    const index = model.moveAtPos(job.pos);
    const lag = LAG_MOVES[job.mode] ?? 16;
    // Nothing has moved yet if the position is still before the first real move.
    const behind = Math.max(index - lag, 0);
    let target = job.pos > 0 ? model.time[behind] : 0;
    this.coast = 0;
    if (job.mode === "sd" && job.state === "running" && job.pos > 0) {
      this.coast = SD_COAST_SECONDS;
      target += Math.min(job.moved_ago, SD_COAST_SECONDS);
    }
    this.target = Math.min(target, model.seconds);
    this.updatedAt = performance.now();
  }

  async loadModel(job) {
    if (this.loading || !this.renderer) return;
    this.loading = true;
    try {
      const resp = await fetch(this.modelURL(job.id));
      if (!resp.ok) {
        this.model = null;
        this.modelMissing = true;
        this.renderer.setModel(null);
      } else {
        const model = new Model(await resp.arrayBuffer());
        this.model = model;
        this.modelMissing = false;
        this.renderer.setModel(model);
        // During a tram check the camera stays on the bed, and frames the print after.
        if (!this.tramDrawn) this.renderer.frame(model);
        this.playhead = Infinity;
      }
      this.modelJob = job.id;
      this.dirty = true;
      this.updateTarget(this.job || job);
      if (this.playhead === Infinity) this.playhead = this.target;
    } catch (err) {
      console.error(err);
    } finally {
      this.loading = false;
    }
  }

  // Glides the drawn head towards where it should be, at the print's own speed, faster if
  // it's behind, and it waits if it's ahead, as a head doesn't print backwards.
  advance(dt) {
    const job = this.job;
    const model = this.model;
    if (!job || !model) return false;
    const before = this.playhead;
    let target = this.target;
    if (this.coast > 0) {
      // Between positions from the card, the target keeps going at the print's own speed.
      const since = (performance.now() - this.updatedAt) / 1000;
      target += Math.min(since, Math.max(this.coast - job.moved_ago, 0));
    }
    target = Math.min(target, model.seconds);
    const gap = target - this.playhead;
    if (Math.abs(gap) > SNAP_SECONDS) {
      this.playhead = target;
    } else if (gap > 0) {
      this.playhead = Math.min(this.playhead + dt * (1 + gap / 2), target);
    }
    return this.playhead !== before;
  }

  tick(now) {
    const dt = Math.min((now - this.lastFrame) / 1000 || 0, 0.25);
    this.lastFrame = now;
    if (this.advance(dt) && now - this.lastDraw >= MIN_DRAW_MS) this.dirty = true;
    // The tram view moves on its own clock, so it's drawn every frame while anything moves.
    const tram = this.tram.frame(performance.now(), dt, this.tolerance) || this.mesh.frame(performance.now(), dt) || this.heights.frame();
    this.frameFor(Boolean(tram));
    if (tram && tram.moving) this.dirty = true;
    if (this.dirty) {
      this.dirty = false;
      this.lastDraw = now;
      const job = this.job;
      const model = this.model;
      const started = job && (job.pos > 0 || job.state === "finished");
      const head = model && started && job.state !== "finished" ? model.at(this.playhead) : null;
      this.renderer.draw({
        head,
        all: job && job.state === "finished",
        showHead: printing(job),
        tram,
      });
      this.placeLabels(tram);
    }
    requestAnimationFrame((t) => this.tick(t));
  }

  resetView() {
    this.renderer.reset();
    this.dirty = true;
  }

  // ---- Mouse and touch ----

  listen() {
    const canvas = this.canvas;
    const renderer = this.renderer;
    const pointers = new Map();
    let pinch = 0;
    let severalFingers = false;
    let lastTap = 0;

    const zoomTo = (distance) => {
      renderer.camera.distance = Math.min(Math.max(distance, MIN_DISTANCE), MAX_DISTANCE);
    };

    canvas.addEventListener("contextmenu", (e) => e.preventDefault());
    canvas.addEventListener("pointerdown", (e) => {
      pointers.set(e.pointerId, { x: e.clientX, y: e.clientY, button: e.button, startX: e.clientX, startY: e.clientY, at: e.timeStamp });
      if (pointers.size > 1) severalFingers = true;
      pinch = 0;
      canvas.setPointerCapture(e.pointerId);
    });
    canvas.addEventListener("pointermove", (e) => {
      const p = pointers.get(e.pointerId);
      if (!p) return;
      const dx = e.clientX - p.x;
      const dy = e.clientY - p.y;
      p.x = e.clientX;
      p.y = e.clientY;
      const cam = renderer.camera;
      renderer.moved = true;
      if (pointers.size >= 2) {
        const [a, b] = [...pointers.values()];
        const span = Math.hypot(a.x - b.x, a.y - b.y);
        if (pinch > 0 && span > 0) zoomTo(cam.distance * (pinch / span));
        pinch = span;
        // Each finger's share of moving both together.
        renderer.pan(dx / 2, dy / 2);
      } else if (p.button === 2 || p.button === 1 || e.shiftKey) {
        renderer.pan(dx, dy);
      } else {
        // The camera goes the other way, so the near side of the model follows the mouse.
        cam.yaw -= dx * 0.006;
        cam.pitch = Math.min(Math.max(cam.pitch + dy * 0.006, -0.1), 1.55);
      }
      this.dirty = true;
    });
    const lift = (e) => {
      const p = pointers.get(e.pointerId);
      pointers.delete(e.pointerId);
      pinch = 0;
      if (p && e.type === "pointerup" && e.pointerType === "touch" && !severalFingers &&
          e.timeStamp - p.at < TAP_MS && Math.hypot(e.clientX - p.startX, e.clientY - p.startY) < TAP_PIXELS) {
        if (e.timeStamp - lastTap < DOUBLE_TAP_MS) {
          lastTap = 0;
          this.resetView();
        } else {
          lastTap = e.timeStamp;
        }
      }
      if (pointers.size === 0) severalFingers = false;
    };
    canvas.addEventListener("pointerup", lift);
    canvas.addEventListener("pointercancel", lift);
    canvas.addEventListener("wheel", (e) => {
      e.preventDefault();
      renderer.moved = true;
      zoomTo(renderer.camera.distance * Math.exp(e.deltaY * 0.001));
      this.dirty = true;
    }, { passive: false });
    canvas.addEventListener("dblclick", () => this.resetView());
    // Safari has its own page zoom, which the view's pinch shouldn't set off.
    for (const name of ["gesturestart", "gesturechange"]) {
      canvas.addEventListener(name, (e) => e.preventDefault(), { passive: false });
    }

    // Turning the phone or opening the details changes the view's shape. A view nobody has
    // moved is framed again to suit, and one they have is left where they put it.
    new ResizeObserver(() => {
      if (!renderer.moved) renderer.reset();
      this.dirty = true;
    }).observe(canvas);

    // A phone can take the GPU back while the page is in the background, and starting again
    // is the simple way to rebuild everything on it.
    canvas.addEventListener("webglcontextlost", (e) => e.preventDefault());
    canvas.addEventListener("webglcontextrestored", () => location.reload());
  }
}
