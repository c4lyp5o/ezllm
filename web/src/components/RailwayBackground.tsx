// Railway-style login backdrop (docx item 15): bezier paths converging into
// a glowing router box with data packets flowing through it — Calypso's own
// animation, adapted to ezllm and raised a notch:
//   - device-pixel-ratio aware (his version was blurry on retina)
//   - ezllm palette (sky accent + sky→violet path tints) instead of rainbow
//   - the box is labelled "ezllm" — it IS the router, so name it
//   - packet comet trails + a slow breathing glow on the box
//   - prefers-reduced-motion renders one static frame
//   - the loop pauses while the tab is hidden (rAF already stops; the clock
//     is time-based so a re-shown tab never fast-forwards)
import { useEffect, useRef } from 'react';
import { useReducedMotion } from '../utils/preferences';

type Seg = { p0: { x: number; y: number }; p1: { x: number; y: number }; p2: { x: number; y: number }; p3: { x: number; y: number } };
type Path = { segments: Seg[]; color: string };
type Packet = { pathIndex: number; t: number; speed: number; size: number; color: string };

const PATH_COLORS = ['#38bdf8', '#60a5fa', '#818cf8', '#22d3ee', '#7dd3fc']; // ezllm sky family

// Packet colours draw from a wider sky→violet range than the rails so two
// packets never read as clones — and explicitly NO white and NO black: the
// old white core was a hard spark against the dark field.
const PACKET_COLORS = ['#38bdf8', '#60a5fa', '#818cf8', '#22d3ee', '#7dd3fc', '#a78bfa', '#c084fc'];

// Restore the busier pre-1.3 desktop scene. Small screens keep a bounded
// backing store and a 30fps budget, with traffic visible from the first paint.
const BUDGET = {
  desktop: { maxPackets: 24, seed: 8, paths: 12, maxDpr: 2, frameMs: 0 },
  phone: { maxPackets: 9, seed: 4, paths: 8, maxDpr: 1.5, frameMs: 33 },
};
const isPhone = () =>
  Math.min(window.innerWidth, window.innerHeight) < 640 || (window.matchMedia?.('(pointer: coarse)').matches ?? false);

export default function RailwayBackground() {
  const canvasRef = useRef<HTMLCanvasElement>(null);
 const reducedMotion=useReducedMotion();

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    let animationFrameId = 0;
    let width = 0;
    let height = 0;
    let paths: Path[] = [];
    let packets: Packet[] = [];
    // Spawn pacing: which track the LAST packet took (so the next one picks a
    // different one) and the rAF timestamp the next packet becomes due.
    let lastPathIndex = -1;
    let nextSpawnAt = 0;
    let budget = BUDGET.desktop;
    let numPaths = budget.paths;
    const boxW = 320;
    const boxH = 160;

    const getCubicBezierXYatT = (p0: Seg['p0'], p1: Seg['p1'], p2: Seg['p2'], p3: Seg['p3'], t: number) => {
      const mt = 1 - t;
      const mt2 = mt * mt;
      const mt3 = mt2 * mt;
      const t2 = t * t;
      const t3 = t2 * t;
      return {
        x: mt3 * p0.x + 3 * mt2 * t * p1.x + 3 * mt * t2 * p2.x + t3 * p3.x,
        y: mt3 * p0.y + 3 * mt2 * t * p1.y + 3 * mt * t2 * p2.y + t3 * p3.y,
      };
    };

    const drawRoundedRect = (c: CanvasRenderingContext2D, x: number, y: number, w: number, h: number, radius: number) => {
      c.beginPath();
      c.moveTo(x + radius, y);
      c.lineTo(x + w - radius, y);
      c.quadraticCurveTo(x + w, y, x + w, y + radius);
      c.lineTo(x + w, y + h - radius);
      c.quadraticCurveTo(x + w, y + h, x + w - radius, y + h);
      c.lineTo(x + radius, y + h);
      c.quadraticCurveTo(x, y + h, x, y + h - radius);
      c.lineTo(x, y + radius);
      c.quadraticCurveTo(x, y, x + radius, y);
      c.closePath();
    };

    // spawnPacket adds ONE packet on a random track the previous packet did
    // not use, in its own colour. atT lets the static (reduced-motion) frame
    // freeze a packet mid-track instead of running the loop.
    const spawnPacket = (atT = 0) => {
      let pathIndex = Math.floor(Math.random() * numPaths);
      if (numPaths > 1) {
        while (pathIndex === lastPathIndex) pathIndex = Math.floor(Math.random() * numPaths);
      }
      lastPathIndex = pathIndex;
      packets.push({
        pathIndex,
        t: atT,
        speed: 0.0018 + Math.random() * 0.0022,
        size: 1.6 + Math.random() * 1.6,
        color: PACKET_COLORS[Math.floor(Math.random() * PACKET_COLORS.length)],
      });
    };

    const init = () => {
      // DPR-aware backing store: CSS pixels stay the layout unit, the canvas
      // gets crisp physical pixels (the retina fix).
      budget = isPhone() ? BUDGET.phone : BUDGET.desktop;
      numPaths = budget.paths;
      const dpr = Math.min(window.devicePixelRatio || 1, budget.maxDpr);
      width = canvas.clientWidth;
      height = canvas.clientHeight;
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

      paths = [];
      packets = [];
      const centerX = width / 2;
      const centerY = height / 2;
      const boxLeft = centerX - boxW / 2;
      const boxRight = centerX + boxW / 2;

      for (let i = 0; i < numPaths; i++) {
        const startY = (height / (numPaths + 1)) * (i + 1) + (Math.random() * 30 - 15);
        const insideY = centerY - boxH / 2 + (boxH / (numPaths + 1)) * (i + 1);
        const endY = (height / (numPaths + 1)) * (i + 1) + (Math.random() * 40 - 20);
        paths.push({
          color: PATH_COLORS[i % PATH_COLORS.length],
          segments: [
            // Left edge converging into the box
            {
              p0: { x: 0, y: startY },
              p1: { x: boxLeft * 0.5, y: startY },
              p2: { x: boxLeft * 0.5, y: insideY },
              p3: { x: boxLeft, y: insideY },
            },
            // Straight/slightly wavy through the router box
            {
              p0: { x: boxLeft, y: insideY },
              p1: { x: boxLeft + boxW * 0.33, y: insideY },
              p2: { x: boxLeft + boxW * 0.66, y: insideY },
              p3: { x: boxRight, y: insideY },
            },
            // Right edge expanding outwards
            {
              p0: { x: boxRight, y: insideY },
              p1: { x: boxRight + (width - boxRight) * 0.5, y: insideY },
              p2: { x: boxRight + (width - boxRight) * 0.5, y: endY },
              p3: { x: width, y: endY },
            },
          ],
        });
      }

      lastPathIndex = -1;
      nextSpawnAt = 0;
      // Seed packets spread along their tracks: the animated page opens with
      // traffic already flowing, and the reduced-motion still frame carries
      // the same picture without running the loop.
      for (let i = 0; i < budget.seed; i++) {
        spawnPacket(0.05 + (0.9 * (i + Math.random())) / budget.seed);
      }
    };

    const posOnPath = (path: Path, t: number) => {
      // Map overall t (0..1) onto the three segments
      let segIdx: number, localT: number;
      if (t < 0.4) { segIdx = 0; localT = t / 0.4; } else if (t < 0.6) { segIdx = 1; localT = (t - 0.4) / 0.2; } else { segIdx = 2; localT = (t - 0.6) / 0.4; }
      const seg = path.segments[segIdx];
      return getCubicBezierXYatT(seg.p0, seg.p1, seg.p2, seg.p3, localT);
    };


    // dt scales packet speed (tuned per 60 Hz frame), so a 120 Hz phone or the
    // 30 fps phone cap moves packets at the same pace on screen.
    const drawFrame = (now: number, dt = 0) => {
      // 1. Motion-blur background trail (deep dark blue fade)
      ctx.globalCompositeOperation = 'source-over';
      ctx.fillStyle = 'rgba(5, 8, 20, 0.16)';
      ctx.fillRect(0, 0, width, height);

      // 2. Static swept paths
      paths.forEach((path) => {
        ctx.beginPath();
        ctx.moveTo(path.segments[0].p0.x, path.segments[0].p0.y);
        path.segments.forEach((seg) => ctx.bezierCurveTo(seg.p1.x, seg.p1.y, seg.p2.x, seg.p2.y, seg.p3.x, seg.p3.y));
        ctx.strokeStyle = path.color;
        ctx.globalAlpha = 0.13;
        ctx.lineWidth = 1.5;
        ctx.stroke();
        ctx.globalAlpha = 1.0;
      });

      // 3. Central router box — labelled, breathing glow
      const centerX = width / 2;
      const centerY = height / 2;
      const boxLeft = centerX - boxW / 2;
      const boxTop = centerY - boxH / 2;
      const breath = reducedMotion ? 20 : 16 + Math.sin(now / 900) * 6;

      drawRoundedRect(ctx, boxLeft, boxTop, boxW, boxH, 20);
      ctx.fillStyle = 'rgba(10, 15, 35, 0.7)';
      ctx.fill();
      ctx.shadowBlur = breath;
      ctx.shadowColor = '#38bdf8';
      ctx.lineWidth = 2;
      ctx.strokeStyle = 'rgba(56, 189, 248, 0.8)';
      ctx.stroke();
      ctx.shadowBlur = 0;

      ctx.fillStyle = 'rgba(125, 211, 252, 0.9)';
      ctx.font = '600 26px ui-monospace, SFMono-Regular, Menlo, monospace';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText('ezllm', centerX, centerY - 6);
      ctx.font = '400 12px ui-monospace, SFMono-Regular, Menlo, monospace';
      ctx.fillStyle = 'rgba(125, 211, 252, 0.45)';
      ctx.fillText('router', centerX, centerY + 18);

      // 4. Packets with comet trails — each spawned at a short random gap on
      // a track the previous packet did not use.
      if (now >= nextSpawnAt && packets.length < budget.maxPackets) {
        spawnPacket();
        nextSpawnAt = now + (budget === BUDGET.phone ? 400 : 180) + Math.random() * 420;
      }
      ctx.globalCompositeOperation = 'screen';
      for (let i = packets.length - 1; i >= 0; i--) {
        const packet = packets[i];
        packet.t += packet.speed * dt;
        if (packet.t >= 1) {
          packets.splice(i, 1); // off the far edge — retire, no respin
          continue;
        }
        const path = paths[packet.pathIndex];
        const pos = posOnPath(path, packet.t);
        const tail = posOnPath(path, Math.max(0, packet.t - 0.012));

        // Trail: a fading line from the packet's recent past to now
        ctx.beginPath();
        ctx.moveTo(tail.x, tail.y);
        ctx.lineTo(pos.x, pos.y);
        ctx.strokeStyle = packet.color;
        ctx.globalAlpha = 0.35;
        ctx.lineWidth = packet.size;
        ctx.lineCap = 'round';
        ctx.stroke();
        ctx.globalAlpha = 1.0;

        // Restore the original desktop comet glow. Phones use the painted
        // halo so the small-screen frame budget stays predictable.
        ctx.fillStyle = packet.color;
        ctx.beginPath();
        ctx.globalAlpha = 0.18;
        ctx.arc(pos.x, pos.y, packet.size + 4, 0, Math.PI * 2);
        ctx.fill();
        ctx.globalAlpha = 1.0;
        ctx.shadowBlur = budget === BUDGET.desktop ? 14 : 0;
        ctx.shadowColor = packet.color;
        ctx.beginPath();
        ctx.arc(pos.x, pos.y, packet.size, 0, Math.PI * 2);
        ctx.fill();
        ctx.shadowBlur = 0;
      }
    };

    let last: number | null = null;
    const animate = (now: number) => {
      animationFrameId = requestAnimationFrame(animate);
      if (last === null) { last = now; return; }
      const elapsed = now - last;
      if (elapsed < budget.frameMs) return;
      drawFrame(now, Math.min(elapsed, 100) / 16.7);
      last = now;
    };

    const handleResize = () => {
      init();
      // Paint independently of the limiter: otherwise a 30fps first frame
      // can repeatedly skip without ever initializing its clock.
      drawFrame(performance.now(), 0);
      last = null;
    };
    const onVisibility = () => {
      cancelAnimationFrame(animationFrameId);
      last = null;
      if (!document.hidden && !reducedMotion) animationFrameId = requestAnimationFrame(animate);
    };
    handleResize();
    if (!reducedMotion && !document.hidden) animationFrameId = requestAnimationFrame(animate);
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(handleResize);
    observer?.observe(canvas);
    window.addEventListener('resize', handleResize);
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      observer?.disconnect();
      window.removeEventListener('resize', handleResize);
      document.removeEventListener('visibilitychange', onVisibility);
      cancelAnimationFrame(animationFrameId);
    };
  }, [reducedMotion]);

  return <canvas ref={canvasRef} aria-hidden className="pointer-events-none absolute inset-0 h-full w-full" />;
}
