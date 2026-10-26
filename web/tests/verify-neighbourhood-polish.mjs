// Verify the data-backed miniature, its nonvisual controls, and degraded states.
import { mkdir, writeFile } from 'node:fs/promises';
import { start, json, check, finish, account } from './mockdash.mjs';
const artifacts='/tmp/ezllm-scene-evidence';await mkdir(artifacts,{recursive:true});
const key=(id,label,hint,enabled=true)=>({id,label,hint,enabled,last_test_ok:true});
const accounts=[
 {...account,id:1,name:'Lantern district',keys:[key(1,'Night shift','a'),key(2,'Weekend worker','b'),key(3,'Retired','c',false)]},
 {...account,id:2,name:'Harbour street',keys:[key(4,'Busy corner','d'),key(5,'Spare room','e')]},
];
const row=(k,calls,tin,errors=0)=>({k,calls,tin,tout:0,errors,cread:0,cwrite:0,saved:0,reasoning:0});
const usage={groups:[row('a',40,120000),row('d',8,5000,4)],totals:{}};
const dash=await start();let pages=0;const errors=[];
async function pageFor(mode='success',options={}){
 const page=await dash.page(async(route,url)=>{
  if(url.pathname==='/admin/accounts'){await json(route,mode==='empty'?[]:accounts,mode==='error'?503:200);return true}
  if(url.pathname==='/admin/usage'){await json(route,usage,mode==='partial'||mode==='error'?503:200);return true}
  return false;
 },options);page.on('pageerror',e=>errors.push(e.message));pages++;return page;
}
try{
 for(const [theme,viewport] of [['dark',{width:1440,height:900}],['light',{width:1440,height:900}],['dark',{width:390,height:844}],['light',{width:390,height:844}]]){
  const page=await pageFor('success',{viewport,hasTouch:viewport.width<640,isMobile:viewport.width<640});
  await page.addInitScript(t=>localStorage.setItem('ezllm.theme',t),theme);
  await page.goto(dash.base+'/neighbourhood');await page.locator('[data-house="1:1"]').waitFor();
  await page.locator('[data-house="1:1"]').focus();await page.keyboard.press('Enter');
  check(`${theme} ${viewport.width}: keyboard selects house`,await page.locator('[data-house="1:1"]').getAttribute('aria-pressed')==='true');
  check(`${theme} ${viewport.width}: inspector shows exact key data`,(await page.getByLabel('Selected house').innerText()).includes('24,000'));
  if(viewport.width<640){await page.locator('[data-house="1:2"]').tap();check(`${theme} ${viewport.width}: touch selects house`,await page.locator('[data-house="1:2"]').getAttribute('aria-pressed')==='true')}
  // The scene keeps wide streets local; it must never widen the page.
  check(`${theme} ${viewport.width}: no page overflow`,await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
  const running=await page.locator('.nbhd-scene').evaluate(e=>e.getAnimations({subtree:true}).filter(a=>a.playState==='running').length);
  check(`${theme} ${viewport.width}: scenery animates`,running>0,running);
  await page.getByRole('button',{name:'Pause scene',exact:true}).click();
  await page.mouse.move(0,0);await page.waitForTimeout(200);
  check(`${theme} ${viewport.width}: pause stops scene animations`,await page.locator('.nbhd-scene').evaluate(e=>e.getAnimations({subtree:true}).every(a=>a.playState!=='running')));
  await page.evaluate(()=>scrollTo(0,0));
  await page.screenshot({path:`${artifacts}/neighbourhood-${theme}-${viewport.width}.png`,fullPage:true});
  await page.getByRole('button',{name:'Resume scene',exact:true}).click();
  await page.emulateMedia({reducedMotion:'reduce'});
  await page.getByRole('button',{name:'Reduced motion',exact:true}).waitFor();
  check(`${theme} ${viewport.width}: reduced motion keeps houses and stops loops`,await page.locator('[data-house]').count()===5&&await page.locator('.nbhd-scene').evaluate(e=>e.getAnimations({subtree:true}).every(a=>a.playState!=='running')));
  const before=await page.locator('.nbhd-scene').screenshot();await page.waitForTimeout(250);const after=await page.locator('.nbhd-scene').screenshot();
  check(`${theme} ${viewport.width}: reduced scene stays still`,before.equals(after));
  await page.context().close();
 }
 const empty=await pageFor('empty');await empty.goto(dash.base+'/neighbourhood');await empty.getByText('No keys yet.',{exact:true}).waitFor();
 check('empty: actionable provider link',await empty.getByRole('link',{name:'Add a provider',exact:true}).getAttribute('href')==='/providers');await empty.screenshot({path:`${artifacts}/neighbourhood-empty.png`});await empty.context().close();
 const partial=await pageFor('partial');await partial.goto(dash.base+'/neighbourhood');await partial.locator('[data-state="unknown"]').first().waitFor();
 check('partial: houses remain, usage is unavailable',await partial.locator('[data-house]').count()===5&&await partial.locator('[data-state="unknown"]').count()===4);
 check('partial: missing usage is never shown as zero',(await partial.locator('table').innerText()).includes('—'));
 await partial.screenshot({path:`${artifacts}/neighbourhood-partial.png`});await partial.context().close();
 const failure=await pageFor('error');await failure.goto(dash.base+'/neighbourhood');await failure.getByRole('alert').waitFor();
 check('error: retry action available',await failure.getByRole('button',{name:'Try again',exact:true}).isEnabled());await failure.screenshot({path:`${artifacts}/neighbourhood-error.png`});
 await failure.route('**/admin/accounts',route=>json(route,accounts));await failure.route('**/admin/usage?*',route=>json(route,usage));
 await failure.getByRole('button',{name:'Try again',exact:true}).click();await failure.locator('[data-house]').first().waitFor();
 check('error: retry restores scene',await failure.locator('[data-house]').count()===5);await failure.context().close();
 let release;const held=new Promise(r=>{release=r});const loading=await dash.page(async(route,url)=>{
  if(url.pathname==='/admin/accounts'){await held;await json(route,accounts);return true}
  if(url.pathname==='/admin/usage'){await json(route,usage);return true}return false;
 });await loading.goto(dash.base+'/neighbourhood');await loading.getByText('Lighting the lamps…',{exact:true}).waitFor();
 await loading.screenshot({path:`${artifacts}/neighbourhood-loading.png`});check('loading: stable feedback visible',await loading.getByRole('status').count()>0);release();await loading.locator('[data-house]').first().waitFor();await loading.context().close();
 let reads=0;const visibility=await dash.page(async(route,url)=>{if(url.pathname==='/admin/accounts'){reads++;await json(route,accounts);return true}if(url.pathname==='/admin/usage'){await json(route,usage);return true}return false});
 await visibility.clock.install();await visibility.goto(dash.base+'/neighbourhood');await visibility.locator('[data-house]').first().waitFor();const initial=reads;
 await visibility.evaluate(()=>{Object.defineProperty(document,'hidden',{configurable:true,get:()=>true});document.dispatchEvent(new Event('visibilitychange'))});
 await visibility.clock.fastForward(11000);check('hidden tab: no polling',reads===initial);check('hidden tab: scene paused',await visibility.locator('.nbhd-scene').getAttribute('data-still')==='true');
 const resumed=visibility.waitForResponse(r=>new URL(r.url()).pathname==='/admin/accounts');
 await visibility.evaluate(()=>{Object.defineProperty(document,'hidden',{configurable:true,get:()=>false});document.dispatchEvent(new Event('visibilitychange'))});await resumed;
 await visibility.waitForFunction(()=>document.querySelector('.nbhd-scene')?.getAttribute('data-still')==='false');
 check('visible tab: polling resumes',reads>initial);await visibility.context().close();
 const long=await pageFor('success',{viewport:{width:390,height:844}});await long.route('**/admin/accounts',r=>json(r,[{...accounts[0],name:'A provider account with a very long descriptive name',keys:[{...accounts[0].keys[0],label:'a'.repeat(120)}]}]));
 await long.goto(dash.base+'/neighbourhood');await long.locator('[data-house]').click();check('long labels: inspector and streets stay within viewport',await long.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await long.screenshot({path:`${artifacts}/neighbourhood-long-label.png`,fullPage:true});await long.context().close();
 const denied=await dash.page(async(route,url)=>{if(url.pathname==='/admin/accounts'||url.pathname==='/admin/usage'){await json(route,{error:{message:'unauthorized',code:401,type:'invalid_request_error'}},401);return true}return false},{unlocked:false});
 await denied.goto(dash.base+'/');await denied.getByRole('heading',{name:'Sign in to ezllm'}).waitFor();await denied.evaluate(()=>sessionStorage.setItem('ezllm.admin.unlocked','1'));
 await denied.goto(dash.base+'/neighbourhood');await denied.getByRole('heading',{name:'Sign in to ezllm'}).waitFor();
 check('permission: expired session returns to login',await denied.locator('[data-house]').count()===0);await denied.context().close();
 check('no browser exceptions',errors.length===0,errors);
 await writeFile(`${artifacts}/checks.json`,JSON.stringify({pages:pages+4,errors},null,2));
}finally{await dash.close()}
finish();
