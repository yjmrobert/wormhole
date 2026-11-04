// Wormhole docs: the wormhole behind the glass. One full-screen triangle, one
// fragment shader, no libraries and no requests. The tunnel is drawn in polar
// space (depth is the inverse of the distance from the throat), its walls are
// layered value noise, and the throat is a lensed window onto the far side,
// where a striped 1984 sun sits on the horizon. Below the horizon the floor
// is the wireframe grid of that night, magenta lines running toward the
// reader and cyan lines across, sagging into the well under the throat.
// The canvas renders below device resolution; the frosted panes above hide
// the difference and keep the frame cheap. It stops when the tab is hidden,
// draws one still frame under prefers-reduced-motion, and removes itself
// (leaving the SVG star field) when WebGL is unavailable.
(function () {
  "use strict";

  var canvas = document.querySelector("canvas.wormhole");
  if (!canvas) return;
  var gl = canvas.getContext("webgl", { alpha: false, antialias: false, depth: false, stencil: false, powerPreference: "low-power", preserveDrawingBuffer: false })
    || canvas.getContext("experimental-webgl", { alpha: false, antialias: false, depth: false, stencil: false });
  if (!gl) { canvas.remove(); return; }

  var vert = [
    "attribute vec2 p;",
    "void main(){ gl_Position = vec4(p, 0.0, 1.0); }"
  ].join("\n");

  var frag = [
    "precision highp float;",
    "uniform vec2 u_res;",
    "uniform float u_time;",
    "uniform vec2 u_center;",
    "uniform vec2 u_drift;",
    "uniform float u_light;",
    "uniform float u_oct;",
    "uniform float u_scan;",
    "#define TAU 6.28318530718",
    "",
    "float hash1(vec3 p){ p = fract(p * 0.3183099 + vec3(0.1, 0.2, 0.3)); p *= 17.0; return fract(p.x * p.y * p.z * (p.x + p.y + p.z)); }",
    "float hash2(vec2 p){ return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }",
    "float noise(vec3 x){",
    "  vec3 i = floor(x); vec3 f = fract(x); f = f * f * (3.0 - 2.0 * f);",
    "  return mix(mix(mix(hash1(i), hash1(i + vec3(1,0,0)), f.x), mix(hash1(i + vec3(0,1,0)), hash1(i + vec3(1,1,0)), f.x), f.y),",
    "             mix(mix(hash1(i + vec3(0,0,1)), hash1(i + vec3(1,0,1)), f.x), mix(hash1(i + vec3(0,1,1)), hash1(i + vec3(1,1,1)), f.x), f.y), f.z);",
    "}",
    "float fbm(vec3 p){",
    "  float a = 0.5, s = 0.0;",
    "  for (int i = 0; i < 6; i++) { if (float(i) >= u_oct) break; s += a * noise(p); p = p * 2.03 + vec3(1.7, 9.2, 3.1); a *= 0.5; }",
    "  return s;",
    "}",
    "float ridge(float n){ return 1.0 - abs(2.0 * n - 1.0); }",
    "",
    "void main(){",
    "  float aspect = u_res.x / u_res.y;",
    "  vec2 frag = gl_FragCoord.xy / u_res;",
    "  vec2 uv = (frag - (u_center + u_drift)) * vec2(aspect, 1.0);",
    "  float t = u_time;",
    "",
    "  vec2 uv0 = uv;",
    "  // the camera rolls slowly as it falls in",
    "  float roll = t * 0.045 + 0.18 * sin(t * 0.11);",
    "  float cs = cos(roll), sn = sin(roll);",
    "  uv = mat2(cs, -sn, sn, cs) * uv;",
    "  float r = length(uv);",
    "  float a = atan(uv.y, uv.x);",
    "  float z = 1.0 / (r + 0.08);",
    "",
    "  // the tunnel bends: the wall's axis wanders with depth and time",
    "  vec2 off = 0.09 * vec2(sin(t * 0.37 + z * 0.45), cos(t * 0.29 + z * 0.4)) * min(z * 0.22, 1.0);",
    "  vec2 uw = uv + off;",
    "  float rw = length(uw);",
    "  float aw = atan(uw.y, uw.x);",
    "  float zw = 1.0 / (rw + 0.08);",
    "  float fog = exp(-zw * 0.11);",
    "",
    "  // walls: smoke, two families of filaments, rings rushing out",
    "  vec3 cyl = vec3(cos(aw) * 2.0, sin(aw) * 2.0, zw * 0.5 - t * 0.85);",
    "  float wall = fbm(cyl * 1.2);",
    "  float fil1 = pow(ridge(fbm(cyl * vec3(2.2, 2.2, 0.9) + 7.0)), 9.0);",
    "  float fil2 = pow(ridge(noise(cyl * vec3(4.0, 4.0, 1.4) + 3.0)), 14.0);",
    "  float phase = zw * 2.4 - t * 2.6;",
    "  float rings = smoothstep(0.94, 1.0, sin(phase)) * smoothstep(0.35, 0.8, noise(vec3(cos(aw) * 3.0, sin(aw) * 3.0, floor(phase / TAU))));",
    "  float dust = pow(noise(vec3(uw * 16.0, t * 0.6)), 9.0);",
    "",
    "  vec3 navy = vec3(0.035, 0.03, 0.085);",
    "  vec3 violet = vec3(0.40, 0.17, 0.80);",
    "  vec3 magenta = vec3(0.96, 0.24, 0.66);",
    "  vec3 cyan = vec3(0.42, 0.92, 1.0);",
    "  vec3 col = navy;",
    "",
    "  // the 1984 sun on the horizon at the far end of the tunnel, behind the smoke",
    "  float hy = -0.05;",
    "  vec2 sun = uv0 - vec2(0.0, 0.03);",
    "  float sr = 0.27;",
    "  float sd = length(sun);",
    "  float sy = clamp((sun.y + sr) / (2.0 * sr), 0.0, 1.0);",
    "  float cut = smoothstep(0.18, 0.26, sin(sun.y * 120.0) + sy * 1.7 - 0.25);",
    "  float sunMask = smoothstep(sr, sr - 0.006, sd) * cut;",
    "  vec3 sunCol = mix(mix(vec3(0.55, 0.2, 0.9), vec3(1.0, 0.3, 0.62), smoothstep(0.0, 0.45, sy)), vec3(1.0, 0.86, 0.5), smoothstep(0.5, 1.0, sy));",
    "  col += sunCol * sunMask * 0.8 * (1.0 - 0.65 * smoothstep(0.25, 0.6, wall));",
    "  col += mix(magenta, vec3(1.0, 0.6, 0.4), 0.4) * smoothstep(sr + 0.16, sr - 0.02, sd) * 0.22;",
    "",
    "  col += violet * wall * wall * 0.95 * fog;",
    "  col += magenta * fil1 * 1.1 * fog;",
    "  col += cyan * fil2 * 1.6 * fog;",
    "  col += mix(cyan, magenta, 0.5 + 0.5 * sin(zw * 0.7 + t)) * rings * 0.85 * fog;",
    "  col += mix(cyan, vec3(1.0), 0.4) * dust * 0.9 * fog;",
    "",
    "  // the wireframe floor below the horizon, sagging into the well",
    "  float below = hy - uv0.y;",
    "  if (below > 0.0) {",
    "    float onFloor = smoothstep(0.0, 0.1, below);",
    "    below += 0.07 * exp(-uv0.x * uv0.x * 14.0) * smoothstep(0.0, 0.35, below);",
    "    float depth = 0.12 / below;",
    "    float px = 1.4 / u_res.y;",
    "    float gx = uv0.x * depth * 5.0;",
    "    float gz = depth * 3.6 + t * 1.5;",
    "    float wx = px * depth * 5.0;",
    "    float wz = px * 0.432 / (below * below);",
    "    float lineX = smoothstep(wx * 1.6, wx * 0.4, abs(fract(gx) - 0.5));",
    "    float lineZ = smoothstep(wz * 1.6, wz * 0.4, abs(fract(gz) - 0.5));",
    "    float fade = onFloor * exp(-depth * 0.22);",
    "    col *= mix(1.0, 0.6, onFloor);",
    "    col += magenta * lineX * fade * 0.95 + cyan * lineZ * fade * 0.55;",
    "    col += magenta * 0.18 * exp(-below * 16.0) * smoothstep(1.2, 0.3, abs(uv0.x));",
    "  }",
    "",
    "  // warp streaks past the camera, outer region only",
    "  float cells = 110.0;",
    "  float ac = floor((a / TAU + 0.5) * cells);",
    "  float hs = hash2(vec2(ac, 3.7));",
    "  float along = log(r + 0.05) * 3.2 - t * (0.7 + hs * 1.3) + hs * 23.0;",
    "  float fa = fract(along);",
    "  float angd = abs(fract((a / TAU + 0.5) * cells) - 0.5);",
    "  float streak = smoothstep(0.0, 0.08, fa) * smoothstep(0.55, 0.08, fa) * smoothstep(0.09, 0.0, angd) * step(0.72, hs);",
    "  col += mix(cyan, vec3(1.0), 0.5) * streak * 0.5 * smoothstep(0.1, 0.45, r);",
    "",
    "  // the far side, seen through the throat with its stars pulled in",
    "  vec2 su = uv * (1.0 + 0.4 / (r + 0.12)) + vec2(t * 0.015, -t * 0.01);",
    "  vec2 sg = floor(su * 42.0); vec2 sf = fract(su * 42.0) - 0.5;",
    "  float sh = hash2(sg);",
    "  vec2 sp = (vec2(hash2(sg + 1.3), hash2(sg + 7.1)) - 0.5) * 0.8;",
    "  float star = smoothstep(0.09, 0.0, length(sf - sp)) * step(0.92, sh) * (0.6 + 0.4 * sin(t * 3.0 + sh * 50.0));",
    "  col += vec3(0.9, 0.96, 1.0) * star * smoothstep(0.5, 0.06, r) * 1.3;",
    "",
    "  // the throat",
    "  float pulse = 0.85 + 0.15 * sin(t * 1.7);",
    "  float glow = 0.016 / (r * r + 0.004) * pulse;",
    "  col += mix(magenta, cyan, smoothstep(0.0, 0.32, r)) * glow * 0.34;",
    "  col += vec3(1.0, 0.96, 1.0) * smoothstep(0.05, 0.0, r) * 0.8 * pulse;",
    "  // a thin cyan event horizon ring",
    "  float horizon = smoothstep(0.012, 0.0, abs(r - 0.16 - 0.01 * sin(t * 1.3)));",
    "  col += cyan * horizon * 0.55;",
    "",
    "  // vignette, tone, grain",
    "  vec2 vg = (frag - 0.5) * vec2(aspect, 1.0);",
    "  col *= 1.0 - 0.6 * smoothstep(0.4, 1.2, length(vg));",
    "  col *= 1.0 - 0.07 * (0.5 + 0.5 * sin(gl_FragCoord.y * TAU / u_scan)) * (1.0 - u_light);",
    "  col = 1.0 - exp(-col * 1.25);",
    "  col += (hash2(gl_FragCoord.xy + fract(t) * 7.0) - 0.5) * 0.018;",
    "",
    "  // light theme: the same scene printed on paper as a cyanotype",
    "  float lum = dot(col, vec3(0.3, 0.59, 0.11));",
    "  vec3 tint = col / max(lum, 0.001);",
    "  vec3 ink = mix(tint, vec3(0.45, 0.72, 1.0), 0.55);",
    "  ink = mix(vec3(dot(ink, vec3(0.3, 0.59, 0.11))), ink, 0.7) * 0.62;",
    "  vec3 paper = vec3(0.975, 0.97, 0.995);",
    "  vec3 onPaper = mix(paper, ink, clamp(lum * 0.95 - 0.03, 0.0, 0.6));",
    "  col = mix(col, onPaper, u_light);",
    "  gl_FragColor = vec4(col, 1.0);",
    "}"
  ].join("\n");

  function compile(type, src) {
    var s = gl.createShader(type);
    gl.shaderSource(s, src);
    gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) { return null; }
    return s;
  }
  var vs = compile(gl.VERTEX_SHADER, vert);
  var fs = compile(gl.FRAGMENT_SHADER, frag);
  if (!vs || !fs) { canvas.remove(); return; }
  var prog = gl.createProgram();
  gl.attachShader(prog, vs); gl.attachShader(prog, fs); gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) { canvas.remove(); return; }
  gl.useProgram(prog);
  var buf = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, buf);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
  var loc = gl.getAttribLocation(prog, "p");
  gl.enableVertexAttribArray(loc);
  gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);
  var U = {};
  ["u_res", "u_time", "u_center", "u_drift", "u_light", "u_oct", "u_scan"].forEach(function (n) { U[n] = gl.getUniformLocation(prog, n); });

  document.documentElement.classList.add("has-wormhole");

  // Resolution and quality. Start below device pixels; step down if frames run long.
  var scale = window.innerWidth > 1400 ? 0.6 : 0.7;
  var octaves = (navigator.hardwareConcurrency && navigator.hardwareConcurrency <= 4) ? 4 : 5;
  var w = 0, h = 0;
  function resize() {
    var dpr = Math.min(window.devicePixelRatio || 1, 1.5);
    var nw = Math.max(1, Math.round(window.innerWidth * dpr * scale));
    var nh = Math.max(1, Math.round(window.innerHeight * dpr * scale));
    if (nw === w && nh === h) return;
    w = nw; h = nh;
    canvas.width = w; canvas.height = h;
    gl.viewport(0, 0, w, h);
  }

  // Theme: 0 is the night, 1 is paper. Follows the switch in site.js smoothly.
  var root = document.documentElement;
  var lightMQ = window.matchMedia("(prefers-color-scheme: light)");
  function wantLight() {
    var t = root.getAttribute("data-theme");
    if (t === "light") return 1; if (t === "dark") return 0;
    return lightMQ.matches ? 1 : 0;
  }
  var light = wantLight();
  var lightTarget = light;
  new MutationObserver(function () { lightTarget = wantLight(); wake(); }).observe(root, { attributes: true, attributeFilter: ["data-theme"] });
  if (lightMQ.addEventListener) lightMQ.addEventListener("change", function () { lightTarget = wantLight(); wake(); });

  // Where the throat sits. On the home page it is the hero window and follows
  // it as the page scrolls; elsewhere it keeps to the right, away from the column.
  var hero = document.querySelector(".hero");
  var cx = 0.5, cy = 0.5;
  function placeThroat() {
    var vw = window.innerWidth, vh = window.innerHeight;
    if (hero) {
      var rc = hero.getBoundingClientRect();
      cx = (rc.left + rc.width * 0.5) / vw;
      cy = 1 - (rc.top + rc.height * 0.52) / vh;
      cy = Math.max(-0.12, Math.min(1.12, cy));
    } else {
      cx = vw > 840 ? 0.8 : 0.58;
      cy = Math.min(0.92, 0.5 + (window.scrollY / vh) * 0.12);
    }
  }

  // Pointer parallax, eased.
  var mx = 0, my = 0, dx = 0, dy = 0;
  var finePointer = window.matchMedia("(pointer: fine)").matches;
  if (finePointer) {
    window.addEventListener("pointermove", function (e) {
      mx = (e.clientX / window.innerWidth - 0.5) * 0.035;
      my = -(e.clientY / window.innerHeight - 0.5) * 0.035;
      wake();
    }, { passive: true });
  }

  var reduceMQ = window.matchMedia("(prefers-reduced-motion: reduce)");
  var start = performance.now();
  var raf = 0, still = false, lastFrame = 0, acc = 0, n = 0;

  function draw(now) {
    resize();
    placeThroat();
    light += (lightTarget - light) * 0.08;
    if (Math.abs(lightTarget - light) < 0.002) light = lightTarget;
    dx += (mx - dx) * 0.06; dy += (my - dy) * 0.06;
    var t = reduceMQ.matches ? 11.0 : (((now - start) / 1000) % 14400);
    gl.uniform2f(U.u_res, w, h);
    gl.uniform1f(U.u_time, t);
    gl.uniform2f(U.u_center, cx, cy);
    gl.uniform2f(U.u_drift, dx, dy);
    gl.uniform1f(U.u_light, light);
    gl.uniform1f(U.u_oct, octaves);
    gl.uniform1f(U.u_scan, Math.max(2.0, 3.0 * (h / window.innerHeight)));
    gl.drawArrays(gl.TRIANGLES, 0, 3);
    canvas.classList.add("live");
  }

  function loop(now) {
    raf = 0;
    if (document.hidden) return;
    draw(now);
    if (lastFrame) {
      acc += now - lastFrame; n++;
      if (n >= 90) {
        var avg = acc / n; acc = 0; n = 0;
        if (avg > 24 && scale > 0.4) { scale *= 0.8; octaves = Math.max(3, octaves - 1); w = 0; }
      }
    }
    lastFrame = now;
    if (reduceMQ.matches) {
      // One still frame, then settle any theme fade and stop.
      if (light !== lightTarget || Math.abs(dx - mx) > 0.0005) raf = requestAnimationFrame(loop);
      else still = true;
      return;
    }
    raf = requestAnimationFrame(loop);
  }
  function wake() { if (!raf && !document.hidden) { lastFrame = 0; still = false; raf = requestAnimationFrame(loop); } }

  window.addEventListener("resize", function () { w = 0; wake(); }, { passive: true });
  window.addEventListener("scroll", function () { if (reduceMQ.matches) wake(); }, { passive: true });
  document.addEventListener("visibilitychange", function () { if (!document.hidden) wake(); });
  if (reduceMQ.addEventListener) reduceMQ.addEventListener("change", function () { start = performance.now(); wake(); });
  canvas.addEventListener("webglcontextlost", function (e) { e.preventDefault(); if (raf) cancelAnimationFrame(raf); raf = 0; }, false);
  canvas.addEventListener("webglcontextrestored", function () { w = 0; wake(); }, false);

  wake();
})();
