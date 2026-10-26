// A 30fps limiter must draw its first frame on a phone/short window.
// Pixels and draw coordinates catch a blank/invalid canvas, not just its DOM node.
import { start, json, check, finish } from './mockdash.mjs';
const dash = await start();
try {
 for (const [name, viewport, reducedMotion] of [
  ['phone', {width:390,height:844}, 'no-preference'],
  ['short desktop', {width:1280,height:600}, 'no-preference'],
  ['desktop', {width:1440,height:900}, 'no-preference'],
  ['reduced motion', {width:390,height:844}, 'reduce'],
 ]) {
  const calls=[]; const errors=[];
  const page=await dash.page(async(route,url)=>{calls.push(url.pathname);if(url.pathname==='/admin/setup'){await json(route,{first_boot:false});return true}return false}, {unlocked:false,viewport,reducedMotion,deviceScaleFactor:2});
  page.on('pageerror',e=>errors.push(e.message));
  await page.addInitScript(()=>{
   window.__railwayArcs=[];
   const arc=CanvasRenderingContext2D.prototype.arc;
   CanvasRenderingContext2D.prototype.arc=function(x,y,...args){window.__railwayArcs.push([x,y]);if(window.__railwayArcs.length>5000)window.__railwayArcs.splice(0,2500);return arc.call(this,x,y,...args)};
  });
  await page.goto(dash.base+'/');
  await page.getByRole('heading',{name:'Sign in to ezllm'}).waitFor();
  await page.waitForTimeout(450);
  const pixels=await page.locator('canvas').evaluate(c=>{
   const ctx=c.getContext('2d'),data=ctx.getImageData(0,0,c.width,c.height).data;
   let visible=0,coloured=0;for(let i=0;i<data.length;i+=4){if(data[i+3]>0)visible++;if(data[i+2]>50||data[i+1]>50)coloured++}
   return {visible,coloured,width:c.width,height:c.height};
  });
  check(name+': railway paints visible rails and packets',pixels.visible>1000&&pixels.coloured>100,pixels);
  const before=await page.locator('canvas').evaluate(c=>c.toDataURL());
  await page.waitForTimeout(200);
  const after=await page.locator('canvas').evaluate(c=>c.toDataURL());
  check(name+': '+(reducedMotion==='reduce'?'static picture remains still':'packets move'),reducedMotion==='reduce'?before===after:before!==after);
  const arcs=await page.evaluate(()=>window.__railwayArcs);
  check(name+': packets stay inside the viewport',arcs.every(([x,y])=>Number.isFinite(x)&&Number.isFinite(y)&&x>=-2&&x<=viewport.width+2&&y>=-2&&y<=viewport.height+2));
  check(name+': no private data requested before login',calls.every(p=>['/admin/setup','/healthz'].includes(p)),calls);
  check(name+': no browser errors',errors.length===0,errors);
  await page.screenshot({path:`/tmp/railway-${name.replaceAll(' ','-')}.png`});
  await page.context().close();
 }
}finally{await dash.close()}
finish();
