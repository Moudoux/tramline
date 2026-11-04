// Tramline's read-only print view. No libraries: raw WebGL 2, as the page is embedded in
// the server's binary and the print server has no outside dependencies.
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
const POLL_MS = 1000;
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

const $ = (id) => document.getElementById(id);

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

    this.buildBed();
    this.buildHead();
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
    const viewProj = multiply(proj, lookAt(eye, this.camera.target, [0, 0, 1]));

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

    if (view.head && view.showHead) this.drawHead(viewProj, view.head.pos);
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

// ---- The page ----

const state = {
  status: null,
  model: null,
  modelJob: null,
  modelMissing: false,
  loading: false,
  // Where the drawn head is, in seconds along the file.
  playhead: 0,
  target: 0,
  coast: 0,
  updatedAt: 0,
  lastFrame: 0,
  lastDraw: 0,
  dirty: true,
};

let renderer = null;
try {
  renderer = new Renderer($("view"));
} catch (err) {
  console.error(err);
  $("no-webgl").hidden = false;
  $("view").hidden = true;
}

function clock(seconds) {
  if (!isFinite(seconds) || seconds < 0) return "–";
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h} h ${m} min`;
  if (m > 0) return `${m} min ${s % 60} s`;
  return `${s} s`;
}

function temp(now, target) {
  if (now === undefined) return "–";
  return target > 0 ? `${now.toFixed(1)} / ${target.toFixed(0)} °C` : `${now.toFixed(1)} °C`;
}

const STATE_TEXT = {
  heating: ["heating", "active", "Heating for the tram check before printing."],
  soaking: ["soaking", "active", "At temperature, letting the bed settle before the tram check."],
  checking: ["checking tram", "active", "Probing the corners of the bed."],
  adjusting: ["needs you", "bad", ""],
  starting: ["starting", "active", "Tram check passed, starting the file."],
  running: ["printing", "active", ""],
  cancelling: ["cancelling", "bad", "The print is being cancelled."],
  finished: ["finished", "good", ""],
  cancelled: ["cancelled", "", ""],
  failed: ["failed", "bad", ""],
  unknown: ["ended, outcome unknown", "", ""],
};

const printing = (job) => job && (job.state === "running" || job.state === "cancelling");
const activeJob = (job) => job && ["heating", "soaking", "checking", "adjusting", "starting", "running", "cancelling"].includes(job.state);

function adjustingText(job) {
  const parts = [];
  const c = job.check;
  if (c && !c.pass) {
    parts.push(`Last tram check: ${c.action} ${c.corner}, turn its knob ${job.turn} seen from above. Range ${c.range_mm.toFixed(3)} mm.`);
  }
  parts.push(job.parked ? "The head is parked. The bed may be hot." : "The head may be low. The bed may be hot.");
  parts.push("Finish with tramline print --continue, or tramline cancel.");
  return parts.join(" ");
}

function showStatus(st) {
  const job = st.print;
  const badge = $("badge");
  let text, kind, message = "";
  if (!st.connected && !activeJob(job)) {
    [text, kind] = ["printer off", ""];
    message = "The printer isn't connected, or its power is off.";
  } else if (activeJob(job)) {
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "active", ""];
    if (job.state === "adjusting") message = adjustingText(job);
  } else if (st.tramming) {
    [text, kind] = ["checking tram", "active"];
  } else if (st.copy) {
    [text, kind] = ["copying to card", "active"];
    const pct = st.copy.total_bytes > 0 ? Math.floor((100 * st.copy.bytes) / st.copy.total_bytes) : 0;
    message = `${st.copy.name}: ${pct}%`;
  } else if (job) {
    [text, kind, message] = STATE_TEXT[job.state] || [job.state, "", ""];
  } else {
    [text, kind] = ["idle", ""];
  }
  badge.textContent = text;
  badge.className = "badge" + (kind ? " " + kind : "");

  $("file").textContent = job ? job.name : "";
  $("nozzle").textContent = st.temps ? temp(st.temps.nozzle, st.temps.nozzle_target) : "–";
  $("bed").textContent = st.temps ? temp(st.temps.bed, st.temps.bed_target) : "–";
  $("version").textContent = `Tramline ${st.version}${st.simulated ? ", simulated printer" : ""}`;

  const notes = [message];
  if (job && job.note) notes.push(job.note);
  if (job && job.stopping) notes.push("Being cancelled.");
  if (job && job.link_lost) notes.push("The link to the printer is lost, so this is out of date.");
  if (job && job.elsewhere) notes.push("Started from the printer, not from here.");
  if (job && state.modelMissing) notes.push("Its file isn't on the Pi, so there's no 3D view.");
  const shown = notes.filter(Boolean).join(" ");
  $("message").textContent = shown;
  $("message").hidden = !shown;
  $("problem").textContent = job && job.error ? job.error : "";
  $("problem").hidden = !(job && job.error);

  const showProgress = job && job.size > 0 && !["heating", "soaking", "checking", "adjusting", "starting"].includes(job.state);
  $("progress-block").hidden = !showProgress;
  if (showProgress) {
    const pct = Math.min((100 * job.pos) / job.size, 100);
    $("bar-fill").style.width = `${pct}%`;
    $("percent").textContent = `${pct.toFixed(pct < 10 ? 1 : 0)}%`;
    $("elapsed").textContent = clock(job.run_seconds);
  }
  $("estimate").textContent = printing(job)
    ? job.mode === "stream"
      ? "Head position is worked out from the lines sent, not measured."
      : "Head position is worked out from the card position, read about every 30 s, not measured."
    : "";
  $("estimate").hidden = !printing(job);
}

// Sets where the head should be from the job's position.
function updateTarget(job) {
  const model = state.model;
  if (!model || !job) return;
  if (job.state === "finished") {
    state.target = model.seconds;
    state.playhead = model.seconds;
    return;
  }
  const index = model.moveAtPos(job.pos);
  const lag = LAG_MOVES[job.mode] ?? 16;
  // Nothing has moved yet if the position is still before the first real move.
  const behind = Math.max(index - lag, 0);
  let target = job.pos > 0 ? model.time[behind] : 0;
  state.coast = 0;
  if (job.mode === "sd" && job.state === "running" && job.pos > 0) {
    state.coast = SD_COAST_SECONDS;
    target += Math.min(job.moved_ago, SD_COAST_SECONDS);
  }
  state.target = Math.min(target, model.seconds);
  state.updatedAt = performance.now();
}

async function loadModel(job) {
  if (state.loading || !renderer) return;
  state.loading = true;
  try {
    const resp = await fetch(`api/model/${job.id}`);
    if (!resp.ok) {
      state.model = null;
      state.modelMissing = true;
      renderer.setModel(null);
    } else {
      const model = new Model(await resp.arrayBuffer());
      state.model = model;
      state.modelMissing = false;
      renderer.setModel(model);
      renderer.frame(model);
      state.playhead = Infinity;
    }
    state.modelJob = job.id;
    state.dirty = true;
    updateTarget(job);
    if (state.playhead === Infinity) state.playhead = state.target;
  } catch (err) {
    console.error(err);
  } finally {
    state.loading = false;
  }
}

async function poll() {
  try {
    const resp = await fetch("api/status", { cache: "no-store" });
    if (!resp.ok) throw new Error(resp.statusText);
    const st = await resp.json();
    state.status = st;
    if (st.print && st.print.id !== state.modelJob) loadModel(st.print);
    showStatus(st);
    updateTarget(st.print);
    showProgressDetails();
    state.dirty = true;
  } catch (err) {
    const badge = $("badge");
    badge.textContent = "can't reach the server";
    badge.className = "badge bad";
  }
  setTimeout(poll, POLL_MS);
}

function showProgressDetails() {
  const job = state.status && state.status.print;
  const model = state.model;
  if (!job || !model) {
    $("layer").textContent = "–";
    $("left").textContent = "–";
    return;
  }
  const at = model.at(state.playhead);
  $("layer").textContent = at ? `${job.state === "finished" ? model.layers : at.layer} of ${model.layers}` : "–";
  $("left").textContent = printing(job) ? `about ${clock(model.seconds - state.playhead)}` : "–";
}

// Glides the drawn head towards where it should be, at the print's own speed, faster if
// it's behind, and it waits if it's ahead, as a head doesn't print backwards.
function advance(dt) {
  const job = state.status && state.status.print;
  const model = state.model;
  if (!job || !model) return false;
  const before = state.playhead;
  let target = state.target;
  if (state.coast > 0) {
    // Between positions from the card, the target keeps going at the print's own speed.
    const since = (performance.now() - state.updatedAt) / 1000;
    target += Math.min(since, Math.max(state.coast - job.moved_ago, 0));
  }
  target = Math.min(target, model.seconds);
  const gap = target - state.playhead;
  if (Math.abs(gap) > SNAP_SECONDS) {
    state.playhead = target;
  } else if (gap > 0) {
    state.playhead = Math.min(state.playhead + dt * (1 + gap / 2), target);
  }
  return state.playhead !== before;
}

function tick(now) {
  const dt = Math.min((now - state.lastFrame) / 1000 || 0, 0.25);
  state.lastFrame = now;
  if (advance(dt) && now - state.lastDraw >= MIN_DRAW_MS) state.dirty = true;
  if (renderer && state.dirty) {
    state.dirty = false;
    state.lastDraw = now;
    const job = state.status && state.status.print;
    const model = state.model;
    const started = job && (job.pos > 0 || job.state === "finished");
    const head = model && started && job.state !== "finished" ? model.at(state.playhead) : null;
    renderer.draw({
      head,
      all: job && job.state === "finished",
      showHead: printing(job),
    });
  }
  requestAnimationFrame(tick);
}

// ---- Mouse and touch ----

if (renderer) {
  const canvas = $("view");
  const pointers = new Map();
  let pinch = 0;
  let severalFingers = false;
  let lastTap = 0;

  const zoomTo = (distance) => {
    renderer.camera.distance = Math.min(Math.max(distance, MIN_DISTANCE), MAX_DISTANCE);
  };
  const resetView = () => {
    renderer.reset();
    state.dirty = true;
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
    state.dirty = true;
  });
  const lift = (e) => {
    const p = pointers.get(e.pointerId);
    pointers.delete(e.pointerId);
    pinch = 0;
    if (p && e.type === "pointerup" && e.pointerType === "touch" && !severalFingers &&
        e.timeStamp - p.at < TAP_MS && Math.hypot(e.clientX - p.startX, e.clientY - p.startY) < TAP_PIXELS) {
      if (e.timeStamp - lastTap < DOUBLE_TAP_MS) {
        lastTap = 0;
        resetView();
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
    state.dirty = true;
  }, { passive: false });
  canvas.addEventListener("dblclick", resetView);
  // Safari has its own page zoom, which the view's pinch shouldn't set off.
  for (const name of ["gesturestart", "gesturechange"]) {
    canvas.addEventListener(name, (e) => e.preventDefault(), { passive: false });
  }

  // Turning the phone or opening the details changes the view's shape. A view nobody has
  // moved is framed again to suit, and one they have is left where they put it.
  new ResizeObserver(() => {
    if (!renderer.moved) renderer.reset();
    state.dirty = true;
  }).observe(canvas);

  // A phone can take the GPU back while the page is in the background, and starting again
  // is the simple way to rebuild everything on it.
  canvas.addEventListener("webglcontextlost", (e) => e.preventDefault());
  canvas.addEventListener("webglcontextrestored", () => location.reload());

  $("more").addEventListener("click", () => {
    const open = $("panel").classList.toggle("open");
    $("more").textContent = open ? "Less" : "Details";
    $("more").setAttribute("aria-expanded", String(open));
  });
}

poll();
requestAnimationFrame(tick);
