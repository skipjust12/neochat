const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const http=require('node:http');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||path.join(__dirname,'../web/markdown-tests/node_modules/playwright'));
const root=path.resolve(__dirname,'..');
const models={models:[
 {id:'gpt-5.4-mini',name:'GPT 5.4 Mini',provider:'openai',tier:'instant',description:'For everyday work',inputs:['text','image']},
 {id:'gpt-6-luna',name:'GPT-6 Luna',provider:'openai',tier:'instant',description:'For complex work',inputs:['text','image'],unstable:true},
 {id:'disabled',name:'Disabled',provider:'openai',tier:'max',unavailable:true,inputs:['text']}
]};
let customModels=[],legacyOverrides={};
const server=http.createServer((req,res)=>{
 const url=new URL(req.url,'http://localhost');
 const json=data=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(data))};
 if(url.pathname==='/models'||url.pathname==='/models/refresh')return json(models);
 if(url.pathname==='/account/models'&&req.method==='POST'){
  let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{const input=JSON.parse(body);assert.equal(input.url,'https://poe.com/Claude-Fable-5.2');assert.equal(input.provider,'anthropic');customModels=[{id:'custom-fable',name:'Claude Fable 5.2',provider:'anthropic',tier:'max',inputs:['text']}];json({models:customModels,legacy_overrides:legacyOverrides})});return;
 }
 if(url.pathname.startsWith('/account/models/')&&req.method==='PATCH'){
  let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>{legacyOverrides[url.pathname.split('/').pop()]=JSON.parse(body).legacy;json({models:customModels,legacy_overrides:legacyOverrides})});return;
 }
 if(url.pathname==='/account/models')return json({models:customModels,legacy_overrides:legacyOverrides});
 if(url.pathname==='/account/models/custom-fable'&&req.method==='DELETE'){customModels=[];return json({models:[],legacy_overrides:legacyOverrides})}

 if(url.pathname==='/auth/session')return json({signed_in:true,user_id:'test'});
 if(url.pathname==='/account/settings')return json({balanceWarnAt:300,balanceLowAt:100});
 if(url.pathname==='/account/balance')return json({current_point_balance:123456,unit:'points'});
 if(url.pathname==='/conversations')return json({conversations:[]});
 if(url.pathname==='/projects')return json({projects:[]});
 if(url.pathname==='/events'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write(': connected\n\n');return}
 const file=url.pathname==='/'?path.join(root,'server/frontend.html'):path.resolve(root,'server','.'+url.pathname);
 if(!file.startsWith(path.join(root,'server')+path.sep)||!fs.existsSync(file)||!fs.statSync(file).isFile()){res.statusCode=404;return res.end()}
 const ext=path.extname(file);res.setHeader('Content-Type',({'.html':'text/html','.js':'text/javascript','.css':'text/css','.json':'application/json','.svg':'image/svg+xml','.woff2':'font/woff2','.ttf':'font/ttf'})[ext]||'application/octet-stream');res.end(fs.readFileSync(file));
});
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH||undefined,args:['--no-sandbox']});
 try{
  const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.addInitScript(()=>{if(window!==window.top)return;localStorage.setItem('neochat-provider-key','pza_old');localStorage.setItem('neochat-image-key','pza_old_media');localStorage.setItem('neochat-preferences-v1',JSON.stringify({balanceWarnAt:300,balanceLowAt:100}))});
  const providerHeaders=[];page.on('request',r=>{const h=r.headers();if(h['x-provider-key']||h['x-image-key'])providerHeaders.push(h)});
  await page.goto('http://127.0.0.1:'+server.address().port,{waitUntil:'domcontentloaded'});
  await page.waitForFunction(()=>window.NeoChatModels&&window.NeoChatModels.current());
  const result=await page.evaluate(()=>{
   const row=document.querySelector('.model-row[data-model-id="gpt-6-luna"]');row.click();
   const disabled=document.querySelector('.model-row[data-model-id="disabled"]');disabled.click();
   return {label:row.querySelector('.model-meta')?.textContent||'',enabled:!row.disabled,chosen:document.getElementById('manualState').dataset.modelId,disabledLabel:disabled.querySelector('.model-meta').textContent,disabled:disabled.disabled,warn:document.getElementById('balanceWarnAt').value,low:document.getElementById('balanceLowAt').value,keyStatus:document.getElementById('providerKeyStatus').textContent};
  });
  assert.equal(await page.locator('.model-row[data-model-id="gpt-5.4-mini"] .model-meta').count(),0);
  assert.equal(result.label,'');assert.equal(result.enabled,true);assert.equal(result.chosen,'gpt-6-luna');assert.equal(result.disabled,true);assert.equal(result.disabledLabel,'N/A');
  assert.equal(result.warn,'10000');assert.equal(result.low,'1000');assert.ok(!result.keyStatus.includes('pza_'));assert.equal(providerHeaders.length,0,'old vendor key was reused');
  await page.waitForFunction(()=>!document.documentElement.classList.contains('is-guest'));
  await page.evaluate(()=>{document.getElementById('customModelProvider').value='anthropic';document.getElementById('customModelURL').value='https://poe.com/Claude-Fable-5.2';document.getElementById('customModelForm').requestSubmit()});
  await page.waitForFunction(()=>document.querySelector('.model-row[data-model-id="custom-fable"]'));
  await page.evaluate(()=>{document.getElementById('manageModelProvider').value='anthropic';document.getElementById('manageModelProvider').dispatchEvent(new Event('change'));document.getElementById('manageModelSelect').value='custom-fable';document.getElementById('manageModelSelect').dispatchEvent(new Event('change'));document.getElementById('modelLegacyToggle').click()});
  await page.waitForFunction(()=>document.querySelector('.model-list[data-legacy] .model-row[data-model-id="custom-fable"]'));
  await page.evaluate(()=>document.querySelector('.model-row[data-model-id="custom-fable"]').click());
  assert.equal(await page.locator('#manualState').getAttribute('data-model-id'),'custom-fable');
  await page.evaluate(()=>window.NeoChatModels.apply({models:[{id:'fresh',name:'Fresh',provider:'openai',tier:'instant',inputs:['text']}]}));
  assert.equal(await page.locator('.model-row[data-model-id="custom-fable"]').count(),1,'refresh lost custom models');
  const cache=await page.evaluate(()=>JSON.parse(localStorage.getItem('neochat-models-poe-v2')));
  assert.ok(!cache.models.some(m=>m.id==='custom-fable'),'private models leaked into shared cache');
  await page.reload({waitUntil:'domcontentloaded'});
  await page.waitForFunction(()=>document.querySelector('.model-row[data-model-id="custom-fable"]'));
  assert.equal(await page.locator('.model-list[data-legacy] .model-row[data-model-id="custom-fable"]').count(),1,'Legacy choice lost on reload');
  await page.evaluate(()=>{document.getElementById('manageModelProvider').value='anthropic';document.getElementById('manageModelProvider').dispatchEvent(new Event('change'));document.getElementById('manageModelSelect').value='custom-fable';document.getElementById('manageModelSelect').dispatchEvent(new Event('change'));document.getElementById('modelLegacyToggle').click()});
  await page.waitForFunction(()=>document.querySelector('.model-list:not([data-legacy]) .model-row[data-model-id="custom-fable"]'));
  await page.evaluate(()=>document.querySelector('#customModelsList button').click());
  await page.waitForFunction(()=>!document.querySelector('.model-row[data-model-id="custom-fable"]'));
  assert.deepEqual(errors,[],'browser errors');
  console.log('Poe UI: exact bot labels, custom add/select/refresh/reload/remove, private cache isolation, disabled N/A, old-key isolation and points migration passed.');
 }finally{await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve))}
})().catch(error=>{console.error(error);process.exitCode=1});
