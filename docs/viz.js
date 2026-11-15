// The visualizer: full-screen scenes in the lights' current colours.
//
// It keeps its own beat clock, corrected ten times a second from the
// program, so motion stays smooth between updates. Each scene is a WebGL
// fragment shader sharing one set of inputs (beat, bar, energy, palette).
// Scenes change on bar boundaries and blend into one another through a
// choice of reveals, each new appearance with a fresh random seed.

const VZ = {
  on: false, gl: null, progs: {}, raf: 0, fs: false, lock: null,
  pos0: 0, t0: 0, period: 500, locked: false, flow: 0, last: 0,
  quality: 0.7, avg: 0.016, refresh: 0.0167, frames: 0, dts: [],
  cols: [[.1, .3, 1], [.4, .2, 1], [.7, .1, .9], [1, .1, .6], [1, .2, .4]], target: null,
  energy: 1, drop: false, wasDrop: false, offbeat: .7, bars: 16,
  fillAt: 0, blockAt: 0, hitAt: -1e9, build: 0, rt: null, post: true, pp: {},
  cur: 0, seed: Math.random(), next: -1, nextSeed: 0, x: 1, xDur: 2000, xStart: 0, style: 0,
  bag: [], lastBar: null, lastChange: 0, hold: false, hudTimer: 0, nameTimer: 0,
};
const hex2rgb = h => [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16) / 255);

function vizSync(st, sent) {
  const v = st.viz, now = performance.now();
  VZ.target = v.colors.map(hex2rgb);
  VZ.energy = st.phrase.known ? st.phrase.energy : 1;
  VZ.drop = v.drop; VZ.offbeat = v.offbeat; VZ.fillAt = v.fill_at || 0; VZ.blockAt = v.block_at || 0;
  if (typeof S === 'object' && S && S.viz_bars !== undefined) VZ.bars = S.viz_bars;
  if (st.locked && v.period_ms > 0) {
    const pos = v.pos + (now - sent) / 2 / v.period_ms;        // allow for the reply's travel time
    const predicted = VZ.pos0 + (now - VZ.t0) / VZ.period;
    const err = pos - predicted;
    VZ.pos0 = (!VZ.locked || Math.abs(err) > 0.3) ? pos : predicted + err * 0.15;
    VZ.t0 = now; VZ.period = v.period_ms; VZ.locked = true;
  } else VZ.locked = false;
  if (!VZ.on) return;
  document.getElementById('vbpm').textContent = st.locked ? st.bpm.toFixed(1) : '--';
  document.getElementById('vphr').textContent = st.phrase.known ? st.phrase.label : (st.locked ? 'Deck ' + st.player : 'Waiting for a deck');
}

// ------------------------------------------------------------------ shaders
const VERT = 'attribute vec2 p; void main(){ gl_Position = vec4(p, 0., 1.); }';

// Shared by every scene.
//   R resolution   T flowing time (faster at higher tempo)   F 0..1 through the beat
//   L how hard this beat hits (0 when nothing is playing)    E energy of the section
//   D 1 during a drop   BAR 0..1 through the bar   BEAT whole beats so far
//   B 0..1 through a build-up (the last bar before a hit)
//   X 0..1 how far this scene has been revealed, XS which reveal, S this showing's seed
//   C the room's five colours
const PRELUDE = `
precision highp float;
uniform vec2 R; uniform float T, F, L, E, D, B, BAR, BEAT, X, XS, S; uniform vec3 C[5];
#define PI 3.14159265
#define TAU 6.2831853
float hash(vec2 p){ p = fract(p * vec2(123.34, 456.21)); p += dot(p, p + 45.32); return fract(p.x * p.y); }
float hash1(float n){ return fract(sin(n * 127.1) * 43758.5453); }
float noise(vec2 p){ vec2 i = floor(p), f = fract(p); f = f*f*(3.-2.*f);
  return mix(mix(hash(i), hash(i+vec2(1,0)), f.x), mix(hash(i+vec2(0,1)), hash(i+vec2(1,1)), f.x), f.y); }
float fbm(vec2 p){ float a = .5, v = 0.; for (int i = 0; i < 5; i++) { v += a*noise(p); p = p*2.03 + vec2(1.7, 9.2); a *= .5; } return v; }
float fbm3(vec2 p){ float a = .5, v = 0.; for (int i = 0; i < 3; i++) { v += a*noise(p); p = p*2.03 + vec2(1.7, 9.2); a *= .5; } return v/.875; }
mat2 rot(float a){ float c = cos(a), s = sin(a); return mat2(c, -s, s, c); }
vec3 pal(float t){                      // the room's colours, there and back so it never jumps
  t = abs(fract(t*.5)*2. - 1.) * 4.;
  float i = floor(t), f = smoothstep(0., 1., fract(t));
  vec3 a = i < 1. ? C[0] : i < 2. ? C[1] : i < 3. ? C[2] : C[3];
  vec3 b = i < 1. ? C[1] : i < 2. ? C[2] : i < 3. ? C[3] : C[4];
  return mix(a, b, f);
}
float roll(){ return B > 0. ? fract(F * (B < .5 ? 2. : 4.)) : F; }   // in a build-up the hits come twice, then four times, a beat
float pulse(){ return exp(-roll()*4.5) * mix(L, 1., B); }      // each hit, fading
vec2 uvc(){ return (gl_FragCoord.xy - .5*R) / min(R.x, R.y); }
float reveal(vec2 uv){                                         // how the incoming scene appears
  if (X >= 1.) return 1.;
  float e;
  if (XS < 1.) e = fbm3(uv*1.6 + S*9.);                                              // dissolves in patches
  else if (XS < 2.) e = clamp(length(uv)*.8, 0., 1.);                                // opens from the centre
  else if (XS < 3.) e = clamp(.5 + .42*dot(uv, vec2(cos(S*TAU), sin(S*TAU))) + .14*(fbm3(uv*3.) - .5), 0., 1.);  // sweeps across
  else e = 1. - clamp(length(uv)*.8, 0., 1.);                                        // closes in from the edges
  return smoothstep(e - .16, e + .16, X*1.32 - .16);
}
vec4 finish(vec3 col, vec2 uv){
  float d = length(uv);
  col *= mix(.5, 1.12, E) * (1. + .3*pulse()) * (1. + .35*B);  // quieter sections sit back; build-ups swell
  col = mix(col, vec3(1.), .16*B*exp(-roll()*6.));             // a white shimmer through the build-up
  col = mix(col, vec3(1.), D * exp(-F*5.) * .85);              // the hit
  col *= 1. - .6*smoothstep(.35, 1.45, d);                     // vignette
  col = col / (1. + .5*col);                                   // soft roll-off instead of clipping
  col = pow(col, vec3(.92));
  return vec4(col, reveal(uv));
}`;

const SCENES = [
  { name: 'Silk', post: { trail: .62, zoom: 1.006, rot: 0, bloom: .7 }, src: `
void main(){                                                   // folds of light, a ring on every beat
  vec2 uv = uvc(); float d = length(uv), pu = pulse();
  vec2 p = uv * (1.45 - .2*pu) + vec2(S*7., .04*sin(BAR*TAU));
  float t = T * .11;
  vec2 q = vec2(fbm3(p + vec2(0., t)), fbm3(p + vec2(5.2, -t*1.3)));
  vec2 r = vec2(fbm3(p + 3.*q + vec2(1.7, 9.2) + .4*t), fbm3(p + 3.*q + vec2(8.3, 2.8) - .3*t));
  float f = fbm(p + 3.5*r);
  float v = smoothstep(.20, .70, f);
  vec3 col = pal(v*.9 + .45*(q.x - .5) + .10*d) * (.07 + 1.5*pow(v, 1.5));
  float ridge = 1. - abs(2.*fbm3(p*1.7 + 4.*r + t) - 1.);
  col += pal(v + .4) * pow(ridge, 8.) * (.35 + .65*v) * (1. + 1.5*pu) + pal(q.y*1.5) * .045;
  float rad = F * 1.35, fade = (1. - F) * (1. - F) * L;
  col += pal(.3 + v*.5) * (exp(-pow((d - rad)/(.010 + .035*F), 2.))*1.7 + exp(-pow((d - rad)/(.07 + .16*F), 2.))*.4) * fade;
  col += pal(v) * pu * .42 / (1. + 8.*d*d);
  vec2 g = uv*9. + vec2(t*2., -t), id = floor(g), gf = fract(g) - .5;
  float h = hash(id);
  col += pal(h*2.) * step(.87, h) * smoothstep(.085, 0., length(gf - (vec2(hash(id + 3.1), hash(id + 7.7)) - .5)*.6)) * (.2 + .8*pu) * (.5 + .5*sin(T*1.7 + h*40.)) * 1.3;
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Tunnel', post: { trail: .55, zoom: 1.022, rot: 0, bloom: .9 }, src: `
void main(){                                                   // flying down a tunnel of lit panels
  vec2 uv = uvc() * rot(.25*sin(T*.07 + S*6.)); float pu = pulse();
  uv += vec2(.06*sin(T*.13), .05*cos(T*.11));
  float r = length(uv) + .001, a = atan(uv.y, uv.x);
  float z = .32/r + T*.85;
  float N = 8. + 2.*floor(S*3.);
  vec2 w = vec2((a/TAU + .06*sin(z*.5 + T*.15) + z*.07*(S - .5)) * N, z*1.4);
  vec2 id = floor(w), f = fract(w);
  float edge = min(min(f.x, 1. - f.x), min(f.y, 1. - f.y));
  float h = hash(id + floor(S*40.));
  float lit = step(.66, fract(h + floor(BEAT)*.381));          // a new set of panels on each beat
  vec3 base = pal(id.y*.06 + h*.3 + T*.02);
  vec3 col = base * (smoothstep(.06, 0., edge)*1.35 + (.035 + lit*(.20 + .95*pu))*(.55 + .45*smoothstep(0., .35, edge)));
  col += pal(id.y*.06 + .5) * smoothstep(.075, 0., edge) * exp(-abs(fract(z*.08 - F*.999) - .5)*14.) * L * .9;   // a wave runs down the walls
  col *= smoothstep(.015, .42, r) * (.75 + .5*r);              // the far end fades into the dark
  col += pal(T*.04) * (.05 + .55*pu) / (1. + 34.*r*r);         // light at the end
  gl_FragColor = finish(col, uvc());
}` },
  { name: 'Kaleidoscope', post: { trail: .66, zoom: 1.004, rot: .004, bloom: .55 }, src: `
void main(){                                                   // mirrored petals, slowly turning
  vec2 uv = uvc(); float pu = pulse();
  float n = 5. + floor(S*4.);
  float r = length(uv), a = atan(uv.y, uv.x) + T*.045 + (S - .5)*floor(BEAT/4.)*.35;
  a = abs(mod(a, TAU/n) - PI/n);
  vec2 q = vec2(cos(a), sin(a)) * r * (1.7 - .28*pu) + vec2(T*.11, sin(T*.07)*.4 + S*5.);
  float v = fbm(q*2. + fbm3(q*3. - T*.1));
  float bands = sin(v*20. - T*.7 + r*5.);
  vec3 col = pal(v*1.7 + r*.45) * (.04 + 1.0*pow(smoothstep(.2, .85, v), 1.6));
  col += pal(v + .5) * smoothstep(.72, 1., bands) * (.18 + .4*v + .7*pu);
  col += pal(.2 + r) * exp(-abs(a)*26.) * (.10 + .5*pu) * smoothstep(.0, .3, r);   // light along the mirrors
  col *= 1. + .7*pu*exp(-r*2.2);
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Aurora', post: { trail: .74, zoom: 1.0, rot: 0, bloom: .8 }, src: `
void main(){                                                   // curtains of light over a starry sky
  vec2 uv = uvc(); float pu = pulse(); vec2 st = gl_FragCoord.xy / R;
  vec3 col = pal(.15) * .035 * (1. - st.y);
  vec2 g = gl_FragCoord.xy / min(R.x, R.y) * 42., id = floor(g);
  float h = hash(id + floor(S*30.));
  col += vec3(.9, .95, 1.) * step(.984, h) * smoothstep(.13, 0., length(fract(g) - .5 - (vec2(hash(id + 1.3), hash(id + 2.7)) - .5)*.5)) * (.45 + .55*sin(T*2. + h*50.)) * .8;
  for (int i = 0; i < 4; i++) {
    float fi = float(i);
    float x = uv.x*(1.1 + .25*fi) + fi*1.7 + S*9.;
    float sway = fbm3(vec2(x*.9 + T*.06*(1. + .3*fi), fi*3. + T*.04))*1.4 - .7;
    float y = uv.y + sway*.42 + .46 - fi*.14;
    float rays = .55 + .45*noise(vec2(x*15. + sway*3., T*.3 + fi));
    float hit = pu * (mod(fi + BEAT, 2.) < 1. ? 1. : .35);     // curtains take turns on the beat
    float body = smoothstep(0., .05, y) * exp(-y*(2.3 - 1.1*hit)) * rays;
    col += pal(fi*.23 + x*.05 + T*.012) * (body*(.5 + .85*hit) + exp(-abs(y)*40.)*.55) * (.95 - .15*fi);
  }
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Horizon', post: { trail: .28, zoom: 1.0, rot: 0, bloom: 1.0 }, src: `
void main(){                                                   // a glowing grid running to a low sun
  vec2 uv = uvc(); float pu = pulse(); vec3 col = vec3(0.);
  float hz = .06 + .03*sin(T*.05 + S*6.);
  if (uv.y < hz) {
    float y = hz - uv.y, z = .26/y;
    vec2 w = vec2(uv.x*z*1.7, z + T*1.15);
    vec2 f = abs(fract(w) - .5);
    float g = min(.5 - f.x, .5 - f.y);
    float line = smoothstep(.028 + .013*z, 0., g) * exp(-z*.11);
    float wave = exp(-abs(z - (1. - F)*15.)*.45) * L;          // each beat rolls in from the horizon
    col += pal(.12 + z*.035) * line * (.75 + 1.5*wave + .6*pu);
    col += pal(.5) * .32 * exp(-y*10.) * (1. + .6*pu);
  } else {
    float y = uv.y - hz;
    vec2 sc = vec2(.0, hz + .215); float sd = length(uv - sc);
    float cut = mix(1., step(.36, fract(y*19. - T*.22)), smoothstep(.25, .03, y));   // bars across the lower sun
    col += mix(pal(.05), pal(.95), clamp(y/.42, 0., 1.)) * smoothstep(.197, .190, sd) * cut * (1.05 + .55*pu);
    col += pal(.8) * .38 * exp(-sd*3.3) * (1. + .9*pu);
    vec2 g = gl_FragCoord.xy / min(R.x, R.y) * 36., id = floor(g); float h = hash(id + floor(S*20.));
    col += vec3(1.) * step(.985, h) * smoothstep(.12, 0., length(fract(g) - .5)) * (.5 + .5*sin(T*2. + h*40.)) * .6 * smoothstep(.05, .3, y);
    col += pal(.5) * .55 * exp(-y*15.);
  }
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Bokeh', post: { trail: .55, zoom: 1.003, rot: 0, bloom: .6 }, src: `
void main(){                                                   // out-of-focus lights drifting in the dark
  vec2 uv = uvc(); float pu = pulse(); vec2 span = R / min(R.x, R.y);
  vec3 col = pal(fbm3(uv*1.2 + T*.03 + S*4.)) * .06 * fbm3(uv*2. - T*.02);
  for (int i = 0; i < 24; i++) {
    float fi = float(i), h = hash1(fi + S*17.), h2 = hash1(fi*1.7 + 3. + S*5.), depth = .35 + .65*hash1(fi*2.3 + 7.);
    vec2 c = (vec2(h, h2) - .5) * span * 1.15 + vec2(sin(T*.11*depth + fi), cos(T*.09*depth + fi*1.3)) * .2 * depth;
    float mine = mod(fi + BEAT, 3.) < 1. ? 1. : 0.;            // a third of them swell on each beat
    float rad = (.045 + .13*depth*depth) * (1. + .3*pu*mine);
    float d = length(uv - c);
    float disc = smoothstep(rad, rad*.84, d);
    float rim = smoothstep(rad*.72, rad*.97, d) * disc;
    col += pal(h*1.7 + T*.012) * (disc*.17 + rim*.33) * (.55 + .45*sin(T*.5 + fi*2.1)) * (.55 + .8*depth) * (1. + 1.1*pu*mine);
  }
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Warp', post: { trail: .78, zoom: 1.03, rot: 0, bloom: .8 }, src: `
void main(){                                                   // stars streaking past
  vec2 uv = uvc() * rot(T*.02*(S - .5)); float pu = pulse();
  float a = atan(uv.y, uv.x)/TAU + .5, r = length(uv);
  vec3 col = pal(fbm3(uv*1.5 + S*3.) + .3) * .09 * fbm3(uv*2.5 - T*.03);
  for (int i = 0; i < 3; i++) {
    float fi = float(i), n = 80. + 70.*fi;
    float sector = floor(a*n), h = hash1(sector + fi*31. + floor(S*10.)*7.);
    float t = fract(h*7. + T*.33*(.25 + .9*h));
    float pos = t*t*1.5, len = (.025 + .24*t) * (.55 + 1.7*pu);  // streaks stretch on the beat
    float along = smoothstep(pos - len, pos, r) * smoothstep(pos + .012, pos, r);
    float across = smoothstep(.5, .12, abs(fract(a*n) - .5));
    col += pal(h*2. + fi*.2) * along * across * (.35 + 1.3*t) * step(.5, h);
  }
  col += pal(T*.02) * (.05 + .4*pu) / (1. + 42.*r*r);
  gl_FragColor = finish(col, uvc());
}` },
  { name: 'Glass', post: { trail: .22, zoom: 1.0, rot: 0, bloom: .7 }, src: `
void main(){                                                   // stained glass whose panes trade places on the beat
  vec2 uv = uvc(); float pu = pulse();
  vec2 q = uv * rot(T*.02 + S*3.) * (3.1 + .4*sin(T*.05));
  vec2 ip = floor(q), fp = fract(q), cid = vec2(0.);
  float d1 = 9., d2 = 9.;
  for (int j = -1; j <= 1; j++) for (int i = -1; i <= 1; i++) {
    vec2 g = vec2(float(i), float(j));
    vec2 o = .5 + .42*sin(T*.25 + TAU*vec2(hash(ip + g), hash(ip + g + 17.3)));
    float d = length(g + o - fp);
    if (d < d1) { d2 = d1; d1 = d; cid = ip + g; } else if (d < d2) { d2 = d; }
  }
  float edge = d2 - d1, h = hash(cid + floor(S*20.));
  float lit = step(.5, fract(h*3.7 + floor(BEAT)*.618));
  vec3 col = pal(h*1.8 + T*.012) * (.06 + (.22 + .95*pu)*lit) * (1.05 - .75*d1);
  col += pal(h*1.8 + .5) * smoothstep(.075, 0., edge) * (.5 + .95*pu);       // light through the leading
  col += pal(h*1.8) * (1. - lit) * .05;
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Liquid', post: { trail: .66, zoom: 1.004, rot: -.003, bloom: .55 }, src: `
void main(){                                                   // bright caustic lines on moving water
  vec2 uv = uvc(); float pu = pulse();
  vec2 q = uv * (2.3 - .25*pu) + S*5.; float t = T*.2;
  for (int i = 1; i < 5; i++) {
    float fi = float(i);
    q += vec2(sin(q.y*fi*1.3 + t)/fi, cos(q.x*fi*1.1 - t*1.2)/fi) * .62;
  }
  float v = .5 + .5*sin(q.x + q.y);
  float lines = abs(sin((q.x - q.y)*3. + t));
  vec3 col = pal(v*1.6 + length(uv)*.35) * (.07 + 1.15*v*v);
  col += pal(v + .5) * pow(1. - lines, 10.) * (.55 + 1.3*pu);
  col += pal(v*.5) * pow(max(sin(q.x*2. - q.y*1.4 + t*1.5), 0.), 20.) * .5;
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Prism', post: { trail: .7, zoom: 1.012, rot: .002, bloom: .9 }, src: `
void main(){                                                   // rays fanning out from a wandering light
  vec2 uv = uvc(); float pu = pulse();
  vec2 c = vec2(.38*sin(T*.07 + S*6.), .22*cos(T*.09 + S*3.));
  vec2 d = uv - c; float r = length(d) + .001, a = atan(d.y, d.x);
  float n = 6. + floor(S*5.);
  float rays = pow(.5 + .5*sin(a*n + T*.22), 4.)*.7 + pow(noise(vec2(a*10. + T*.12, T*.18 + S*9.)), 2.)*1.1 + pow(noise(vec2(a*31. - T*.2, 3.)), 3.)*.6;
  vec3 col = pal(a/TAU*2. + T*.02 + r*.35) * rays * (.22 + .95*pu) * exp(-r*1.15);
  col += pal(.5 + r*.8) * (.45 + 1.3*pu) * exp(-r*10.);
  col += pal(a/TAU*4. + .3) * exp(-abs(sin(a*2. + T*.05))*38.) * exp(-r*1.8) * (.35 + .6*pu);
  col += pal(.7 + a/TAU) * exp(-pow((r - roll()*1.2)/(.02 + .05*roll()), 2.)) * (1. - roll()) * L * .9;   // a halo leaves the light on each hit
  col += pal(.2) * .05 * fbm3(uv*3. + T*.05);
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Ribbons', post: { trail: .72, zoom: 1.002, rot: 0, bloom: 1.0 }, src: `
void main(){                                                   // neon ribbons riding slow waves
  vec2 uv = uvc(); float pu = pulse(); vec3 col = pal(.1) * .015;
  for (int i = 0; i < 7; i++) {
    float fi = float(i), h = hash1(fi + S*13.);
    float mine = mod(fi + BEAT, 2.) < 1. ? 1. : .28;           // alternate ribbons jump on each beat
    float amp = (.10 + .12*h) * (1. + .55*pu*mine);
    float y = (fi - 3.)*.085 + amp*sin(uv.x*(1.6 + 1.4*h) + T*(.35 + .5*h) + fi*1.3) + .035*sin(uv.x*7. - T*.9 + fi*2.);
    float d = abs(uv.y - y);
    col += pal(fi*.17 + uv.x*.18 + T*.015) * (.0045/(d + .0045) + .028/(d + .05)) * (.5 + .65*pu*mine);
  }
  gl_FragColor = finish(col, uv);
}` },
  { name: 'Hive', post: { trail: .4, zoom: 1.0, rot: 0, bloom: .8 }, src: `
void main(){                                                   // a honeycomb with a ripple crossing it on each beat
  vec2 uv = uvc(); float pu = pulse(), zoom = 5.5 + S*2.;
  vec2 p = uv * rot(T*.015 + S*2.) * zoom;
  vec2 sz = vec2(1., 1.7320508);
  vec4 hc = floor(vec4(p, p - vec2(.5, 1.)) / sz.xyxy) + .5;
  vec4 h = vec4(p - hc.xy*sz, p - (hc.zw + .5)*sz);
  vec4 cell = dot(h.xy, h.xy) < dot(h.zw, h.zw) ? vec4(h.xy, hc.xy) : vec4(h.zw, hc.zw + .5);
  vec2 q = abs(cell.xy); float ed = max(dot(q, sz*.5), q.x);   // 0 at a cell's centre, .5 at its edge
  float dc = length(p - cell.xy) / zoom, hs = hash(cell.zw + floor(S*30.));
  float ripple = exp(-abs(dc - roll()*1.25)*7.) * (1. - roll()*.7) * mix(L, 1., B);
  float lit = step(.78, fract(hs + floor(BEAT)*.27));
  vec3 col = pal(hs*.5 + dc*.9 + T*.012) * (.07 + ripple*1.1 + lit*(.30 + .5*pu) + .10*fbm3(cell.zw*.35 + T*.08)) * smoothstep(.5, .40, ed) * (1.1 - ed*.9);
  col += pal(dc*.9 + .5) * smoothstep(.43, .5, ed) * (.18 + .55*ripple + .25*pu);
  gl_FragColor = finish(col, uv);
}` },
];

// Three small passes after the scene: a glow around bright areas, light that
// lingers and drifts (trails), and colour fringing that kicks with the beat.
const POST_BLOOM = `
precision highp float; uniform sampler2D A; uniform vec2 R;
void main(){ vec2 uv = gl_FragCoord.xy / R; vec3 s = vec3(0.); float ws = 0.;
  for (int j = -2; j <= 2; j++) for (int i = -2; i <= 2; i++) {
    float w = exp(-float(i*i + j*j) * .35);
    s += max(texture2D(A, uv + vec2(float(i), float(j)) * 2.2 / R).rgb - .42, 0.) * 1.8 * w; ws += w; }
  gl_FragColor = vec4(s / ws, 1.); }`;
const POST_MIX = `
precision highp float; uniform sampler2D A, G, P; uniform vec2 R; uniform float TRAIL, ZOOM, ROT, BLOOM, CHROMA;
void main(){ vec2 uv = gl_FragCoord.xy / R, c = uv - .5;
  vec3 cur = vec3(texture2D(A, uv + c*CHROMA).r, texture2D(A, uv).g, texture2D(A, uv - c*CHROMA).b);
  cur += texture2D(G, uv).rgb * BLOOM;
  vec2 q = c; q.x *= R.x / R.y;                                // turn and scale in square space
  q = mat2(cos(ROT), -sin(ROT), sin(ROT), cos(ROT)) * q / ZOOM; q.x *= R.y / R.x;
  gl_FragColor = vec4(max(cur, texture2D(P, q + .5).rgb * TRAIL), 1.); }`;
const POST_OUT = `
precision highp float; uniform sampler2D A; uniform vec2 R; uniform float SEED;
float hash(vec2 p){ p = fract(p * vec2(123.34, 456.21)); p += dot(p, p + 45.32); return fract(p.x * p.y); }
void main(){ vec3 col = texture2D(A, gl_FragCoord.xy / R).rgb;
  col += (hash(gl_FragCoord.xy + SEED) - .5) * .022;           // grain hides banding
  gl_FragColor = vec4(col, 1.); }`;

function vizCompile(name, frag, uniforms) {
  const gl = VZ.gl;
  const sh = (type, src) => { const s = gl.createShader(type); gl.shaderSource(s, src); gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error(name + ': ' + gl.getShaderInfoLog(s)); return s; };
  const prog = gl.createProgram();
  gl.attachShader(prog, sh(gl.VERTEX_SHADER, VERT)); gl.attachShader(prog, sh(gl.FRAGMENT_SHADER, frag));
  gl.bindAttribLocation(prog, 0, 'p');
  gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) throw new Error(name + ': ' + gl.getProgramInfoLog(prog));
  const u = {};
  for (const n of uniforms) u[n] = gl.getUniformLocation(prog, n === 'C' ? 'C[0]' : n);
  return { prog, u };
}

// Off-screen surfaces for the passes; rebuilt when the size changes.
function vizTargets(w, h) {
  const gl = VZ.gl;
  if (VZ.rt && VZ.rt.w === w && VZ.rt.h === h) return true;
  if (VZ.rt) for (const t of [VZ.rt.scene, VZ.rt.glow, ...VZ.rt.keep]) { gl.deleteTexture(t.tex); gl.deleteFramebuffer(t.fbo); }
  const mk = (tw, th) => {
    const tex = gl.createTexture(); gl.bindTexture(gl.TEXTURE_2D, tex);
    gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, tw, th, 0, gl.RGBA, gl.UNSIGNED_BYTE, null);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE); gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
    const fbo = gl.createFramebuffer(); gl.bindFramebuffer(gl.FRAMEBUFFER, fbo);
    gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, tex, 0);
    const ok = gl.checkFramebufferStatus(gl.FRAMEBUFFER) === gl.FRAMEBUFFER_COMPLETE;
    gl.clearColor(0, 0, 0, 1); gl.clear(gl.COLOR_BUFFER_BIT);
    return { tex, fbo, w: tw, h: th, ok };
  };
  const rt = { w, h, scene: mk(w, h), glow: mk(Math.max(2, w >> 2), Math.max(2, h >> 2)), keep: [mk(w, h), mk(w, h)], i: 0 };
  gl.bindFramebuffer(gl.FRAMEBUFFER, null);
  VZ.rt = rt;
  return rt.scene.ok && rt.glow.ok && rt.keep[0].ok && rt.keep[1].ok;
}

function vizProgram(i) {
  if (!VZ.progs[i]) VZ.progs[i] = vizCompile(SCENES[i].name, PRELUDE + SCENES[i].src, ['R', 'T', 'F', 'L', 'E', 'D', 'B', 'BAR', 'BEAT', 'X', 'XS', 'S', 'C']);
  return VZ.progs[i];
}

function vizInit() {
  const cv = document.getElementById('vizc');
  const gl = cv.getContext('webgl', { antialias: false, alpha: false, powerPreference: 'high-performance' });
  if (!gl) return false;
  VZ.gl = gl;
  gl.bindBuffer(gl.ARRAY_BUFFER, gl.createBuffer());
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);
  gl.enableVertexAttribArray(0); gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0);
  gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
  vizProgram(VZ.cur);
  try {
    VZ.pp = { bloom: vizCompile('glow', POST_BLOOM, ['A', 'R']), mix: vizCompile('trails', POST_MIX, ['A', 'G', 'P', 'R', 'TRAIL', 'ZOOM', 'ROT', 'BLOOM', 'CHROMA']), out: vizCompile('output', POST_OUT, ['A', 'R', 'SEED']) };
  } catch (e) { console.error(e); VZ.post = false; }           // without them the scenes still draw, just plainer
  // build the other scenes one at a time in the background, so changes never stutter
  let i = 0; const warm = () => { if (i < SCENES.length) { try { vizProgram(i++); } catch (e) { console.error(e); } setTimeout(warm, 250); } };
  setTimeout(warm, 600);
  return true;
}

// ------------------------------------------------------------ scene changes
function vizPick() {                    // every scene once, in a fresh order, before any repeats
  if (!VZ.bag.length) {
    VZ.bag = SCENES.map((_, i) => i).filter(i => i !== VZ.cur);
    for (let i = VZ.bag.length - 1; i > 0; i--) { const j = Math.floor(Math.random() * (i + 1)); [VZ.bag[i], VZ.bag[j]] = [VZ.bag[j], VZ.bag[i]]; }
  }
  return VZ.bag.pop();
}

function vizGo(i, ms, style) {
  if (i === VZ.cur && VZ.next < 0) return;
  try { vizProgram(i); } catch (e) { console.error(e); return; }
  if (VZ.next >= 0) { VZ.cur = VZ.next; VZ.seed = VZ.nextSeed; }     // finish any change already under way
  VZ.next = i; VZ.nextSeed = Math.random(); VZ.x = 0; VZ.xStart = performance.now(); VZ.xDur = ms;
  VZ.style = style === undefined ? Math.floor(Math.random() * 4) : style;
  VZ.lastChange = VZ.xStart;
  const n = document.getElementById('vscene');
  n.textContent = SCENES[i].name + (VZ.hold ? ' · held' : ''); n.classList.add('show');
  clearTimeout(VZ.nameTimer); VZ.nameTimer = setTimeout(() => n.classList.remove('show'), 3200);
}

function vizAuto(now, pos) {
  // The program reports the next build-up hit and scene line as beat
  // positions. The lights get there a little before the sound does, so the
  // program moves on to the following one early; hold on to each target
  // until the sound reaches it.
  if (VZ.locked) {
    if (VZ.fillAt > pos + 0.5) VZ.nextHit = VZ.fillAt;
    if (VZ.blockAt > pos + 0.5) VZ.nextLine = VZ.blockAt;
  } else { VZ.nextHit = VZ.nextLine = undefined; }

  VZ.build = 0;
  if (VZ.nextHit !== undefined) {
    const left = VZ.nextHit - pos;
    if (left <= 0) { VZ.hitAt = VZ.nextHit; VZ.nextHit = undefined; }               // the hit lands now
    else if (left <= 4) VZ.build = 1 - left / 4;                                    // the bar before it builds
  }
  const hit = VZ.locked && pos - VZ.hitAt >= 0 && pos - VZ.hitAt < 1;
  VZ.hitNow = hit || (VZ.drop && VZ.locked);

  let line = false;
  if (VZ.nextLine !== undefined && pos >= VZ.nextLine) { line = true; VZ.nextLine = undefined; }
  if (VZ.hold || VZ.next >= 0 || VZ.bars <= 0) return;
  if (VZ.locked) {
    if (VZ.hitNow && !VZ.wasDrop) {                                                 // a hit cuts straight to a new scene
      VZ.wasDrop = true;
      if (now - VZ.lastChange > 4000) { vizGo(vizPick(), 350, 1); return; }
    }
    if (!VZ.hitNow) VZ.wasDrop = false;
    if (line && now - VZ.lastChange > 4000) vizGo(vizPick(), Math.max(1400, Math.min(3600, VZ.period * 4)));   // otherwise blend over one bar
  } else if (now - VZ.lastChange > 30000) vizGo(vizPick(), 3500);                    // nothing playing: drift on
}

// ------------------------------------------------------------------- render
function vizDraw(i, seed, x, f) {
  const P = VZ.progs[i], gl = VZ.gl, u = P.u;
  gl.useProgram(P.prog);
  gl.uniform2f(u.R, f.w, f.h); gl.uniform1f(u.T, VZ.flow % 2000); gl.uniform1f(u.F, f.F); gl.uniform1f(u.L, f.L);
  gl.uniform1f(u.E, VZ.energy); gl.uniform1f(u.D, VZ.hitNow ? 1 : 0); gl.uniform1f(u.B, VZ.build); gl.uniform1f(u.BAR, f.bar); gl.uniform1f(u.BEAT, f.beat);
  gl.uniform1f(u.X, x); gl.uniform1f(u.XS, VZ.style); gl.uniform1f(u.S, seed); gl.uniform3fv(u.C, VZ.cols.flat());
  gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
}

function vizFrame(now) {
  if (!VZ.on) return;
  VZ.raf = requestAnimationFrame(vizFrame);
  const gl = VZ.gl, cv = gl.canvas;
  // Render a fixed budget of pixels whatever the screen's shape (so an
  // ultrawide is as sharp as a 16:9 screen), never above native, and shrink
  // the budget on machines that cannot hold the display's frame rate.
  const dpr = Math.min(devicePixelRatio || 1, 2);
  const scale = Math.min(dpr, Math.sqrt(1.5e6 * VZ.quality / Math.max(1, cv.clientWidth * cv.clientHeight)));
  const w = Math.max(2, Math.round(cv.clientWidth * scale)), h = Math.max(2, Math.round(cv.clientHeight * scale));
  if (cv.width !== w || cv.height !== h) { cv.width = w; cv.height = h; }
  const post = VZ.post && vizTargets(w, h);
  const dt = Math.min(0.1, (now - (VZ.last || now)) / 1000); VZ.last = now;
  if (dt > 0.003 && dt < 0.08) {                               // ignore a throttled background tab
    VZ.avg += (dt - VZ.avg) * 0.05;
    VZ.dts.push(dt); if (VZ.dts.length > 80) VZ.dts.shift();
  }
  if (++VZ.frames % 40 === 0 && VZ.dts.length >= 40) {
    // The display's own frame time: the quick end of recent frames, but not
    // the single quickest, which can be a fluke.
    VZ.refresh = Math.max(1 / 240, VZ.dts.slice().sort((a, b) => a - b)[Math.floor(VZ.dts.length * 0.15)]);
    // Aim for the display's rate (60, 85, 120 Hz...), not a fixed number.
    if (VZ.avg > VZ.refresh * 1.3 && VZ.quality > 0.2) VZ.quality = Math.max(0.2, VZ.quality * 0.8);
    else if (VZ.avg < VZ.refresh * 1.1 && VZ.quality < 1) VZ.quality = Math.min(1, VZ.quality * 1.08);
  }
  const pos = VZ.pos0 + (now - VZ.t0 + 25) / VZ.period;          // 25 ms for the screen itself
  const beat = Math.floor(pos), inBar = ((beat % 4) + 4) % 4;
  const f = { w, h, F: VZ.locked ? pos - beat : 1, L: VZ.locked ? (inBar === 0 ? 1 : Math.max(0.35, VZ.offbeat)) : 0,
    bar: (((pos % 4) + 4) % 4) / 4, beat: ((beat % 4096) + 4096) % 4096 };
  VZ.flow += dt * (VZ.locked ? 500 / VZ.period : 0.45);          // drifts faster at higher tempo
  if (VZ.target) for (let i = 0; i < 5; i++) for (let k = 0; k < 3; k++) VZ.cols[i][k] += (VZ.target[i][k] - VZ.cols[i][k]) * Math.min(1, dt * 5);

  vizAuto(now, pos);
  gl.bindFramebuffer(gl.FRAMEBUFFER, post ? VZ.rt.scene.fbo : null);
  gl.viewport(0, 0, w, h);
  gl.disable(gl.BLEND);
  vizDraw(VZ.cur, VZ.seed, 1, f);
  let look = SCENES[VZ.cur].post;
  if (VZ.next >= 0) {
    VZ.x = Math.min(1, (now - VZ.xStart) / VZ.xDur);
    const eased = VZ.x * VZ.x * (3 - 2 * VZ.x);
    gl.enable(gl.BLEND);
    vizDraw(VZ.next, VZ.nextSeed, Math.min(eased, 0.9999), f);
    gl.disable(gl.BLEND);
    const to = SCENES[VZ.next].post, m = (a, b) => a + (b - a) * eased;
    look = { trail: m(look.trail, to.trail), zoom: m(look.zoom, to.zoom), rot: m(look.rot, to.rot), bloom: m(look.bloom, to.bloom) };
    if (VZ.x >= 1) { VZ.cur = VZ.next; VZ.seed = VZ.nextSeed; VZ.next = -1; }
  }
  if (post) {
    const rt = VZ.rt, pp = VZ.pp, kick = Math.exp(-(VZ.build > 0 ? (f.F * (VZ.build < 0.5 ? 2 : 4)) % 1 : f.F) * 4.5) * (VZ.build > 0 ? 1 : f.L);
    const tex = (unit, t, loc) => { gl.activeTexture(gl.TEXTURE0 + unit); gl.bindTexture(gl.TEXTURE_2D, t); gl.uniform1i(loc, unit); };
    // glow, at a quarter of the size
    gl.bindFramebuffer(gl.FRAMEBUFFER, rt.glow.fbo); gl.viewport(0, 0, rt.glow.w, rt.glow.h);
    gl.useProgram(pp.bloom.prog); tex(0, rt.scene.tex, pp.bloom.u.A); gl.uniform2f(pp.bloom.u.R, rt.glow.w, rt.glow.h);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    // this frame over a faded, drifting copy of the last one
    const from = rt.keep[rt.i], into = rt.keep[1 - rt.i];
    gl.bindFramebuffer(gl.FRAMEBUFFER, into.fbo); gl.viewport(0, 0, w, h);
    gl.useProgram(pp.mix.prog); tex(0, rt.scene.tex, pp.mix.u.A); tex(1, rt.glow.tex, pp.mix.u.G); tex(2, from.tex, pp.mix.u.P);
    gl.uniform2f(pp.mix.u.R, w, h);
    gl.uniform1f(pp.mix.u.TRAIL, Math.pow(look.trail, Math.max(0.25, dt * 60)));   // the same fade at any frame rate
    gl.uniform1f(pp.mix.u.ZOOM, 1 + (look.zoom - 1) * Math.max(0.25, dt * 60) + 0.010 * kick + 0.012 * VZ.build);
    gl.uniform1f(pp.mix.u.ROT, look.rot * Math.max(0.25, dt * 60));
    gl.uniform1f(pp.mix.u.BLOOM, look.bloom * (1 + 0.5 * kick));
    gl.uniform1f(pp.mix.u.CHROMA, 0.0012 + 0.006 * kick + 0.006 * VZ.build);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    rt.i = 1 - rt.i;
    // to the screen
    gl.bindFramebuffer(gl.FRAMEBUFFER, null); gl.viewport(0, 0, w, h);
    gl.useProgram(pp.out.prog); tex(0, into.tex, pp.out.u.A); gl.uniform2f(pp.out.u.R, w, h); gl.uniform1f(pp.out.u.SEED, (now % 1000) * 0.37);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    gl.activeTexture(gl.TEXTURE0);
  }
  document.querySelectorAll('.vdots i').forEach((e, i) => e.classList.toggle('on', VZ.locked && inBar === i && f.F < 0.5));
}

// ----------------------------------------------------------------- controls
function vizHud() {
  const box = document.getElementById('viz'); box.classList.add('hud');
  clearTimeout(VZ.hudTimer); VZ.hudTimer = setTimeout(() => box.classList.remove('hud'), 3500);
}

function vizStep(dir) {
  const from = VZ.next >= 0 ? VZ.next : VZ.cur;
  vizGo((from + dir + SCENES.length) % SCENES.length, 900);
}

function vizHold(on) {
  VZ.hold = on === undefined ? !VZ.hold : on;
  const n = document.getElementById('vscene');
  n.textContent = SCENES[VZ.next >= 0 ? VZ.next : VZ.cur].name + (VZ.hold ? ' · held' : ' · changing');
  n.classList.add('show'); clearTimeout(VZ.nameTimer); VZ.nameTimer = setTimeout(() => n.classList.remove('show'), 2500);
}

async function vizOpen(fullscreen) {
  const box = document.getElementById('viz'), msg = document.getElementById('vmsg');
  box.classList.remove('hidden'); msg.classList.add('hidden');
  try { if (!VZ.gl && !vizInit()) throw new Error('This browser has no WebGL.'); }
  catch (e) { msg.textContent = 'The visualizer could not start: ' + e.message; msg.classList.remove('hidden'); box.classList.add('hud'); return; }
  VZ.on = true; VZ.last = 0; VZ.lastBar = null; VZ.lastChange = performance.now(); vizHud();
  if (fullscreen && box.requestFullscreen) { try { await box.requestFullscreen(); VZ.fs = true; } catch (e) { VZ.fs = false; } }
  try { VZ.lock = await navigator.wakeLock.request('screen'); } catch (e) {}   // keep the screen awake
  cancelAnimationFrame(VZ.raf); VZ.raf = requestAnimationFrame(vizFrame);
  if (location.hash !== '#viz') history.replaceState(null, '', '#viz');
}

function vizClose() {
  VZ.on = false; cancelAnimationFrame(VZ.raf);
  document.getElementById('viz').classList.add('hidden');
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
  if (VZ.lock) { VZ.lock.release().catch(() => {}); VZ.lock = null; }
  VZ.fs = false; history.replaceState(null, '', location.pathname + location.search);
}

function vizWire() {
  const box = document.getElementById('viz');
  document.getElementById('vopen').onclick = () => vizOpen(true);
  document.getElementById('vclose').onclick = e => { e.stopPropagation(); vizClose(); };
  document.getElementById('vprev').onclick = e => { e.stopPropagation(); vizStep(-1); vizHud(); };
  document.getElementById('vnext').onclick = e => { e.stopPropagation(); vizStep(1); vizHud(); };
  document.getElementById('vhold').onclick = e => { e.stopPropagation(); vizHold(); e.target.textContent = VZ.hold ? 'Resume changes' : 'Hold scene'; vizHud(); };
  box.addEventListener('pointermove', vizHud);
  let sx = null;
  box.addEventListener('pointerdown', e => { sx = e.clientX; vizHud(); });
  box.addEventListener('pointerup', e => { if (sx !== null && Math.abs(e.clientX - sx) > 60) vizStep(e.clientX < sx ? 1 : -1); sx = null; });   // swipe
  document.addEventListener('keydown', e => {
    if (!VZ.on) return;
    if (e.key === 'Escape') vizClose();
    else if (e.key === 'ArrowRight') vizStep(1);
    else if (e.key === 'ArrowLeft') vizStep(-1);
    else if (e.key === ' ' || e.key === 'l' || e.key === 'L') { e.preventDefault(); vizHold(); document.getElementById('vhold').textContent = VZ.hold ? 'Resume changes' : 'Hold scene'; }
    else if (e.key >= '1' && e.key <= '9' && Number(e.key) <= SCENES.length) vizGo(Number(e.key) - 1, 900);
  });
  document.addEventListener('fullscreenchange', () => { if (VZ.on && VZ.fs && !document.fullscreenElement) vizClose(); });
  document.addEventListener('visibilitychange', async () => { if (VZ.on && !document.hidden && !VZ.lock) { try { VZ.lock = await navigator.wakeLock.request('screen'); } catch (e) {} } });
  if (location.hash === '#viz') vizOpen(false);   // an address ending in #viz opens straight into it
}
