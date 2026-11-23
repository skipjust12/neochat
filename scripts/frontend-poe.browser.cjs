const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const http=require('node:http');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||path.join(__dirname,'../web/markdown-tests/node_modules/playwright'));
const root=path.resolve(__dirname,'..');
const models={models:[
 {id:'gpt-5.4-mini',name:'GPT 5.4 Mini',provider:'openai',tier:'instant',url:'https://poe.com/GPT-5.4-mini',inputs:['text','image']},
 {id:'gpt-6-luna',name:'GPT-6 Luna',provider:'openai',tier:'instant',url:'https://poe.com/GPT-6-Luna',inputs:['text','image'],unstable:true},
 {id:'disabled',name:'Disabled',provider:'openai',tier:'max',url:'https://poe.com/Disabled',unavailable:true,inputs:['text']}
]};
let customModels=[],legacyOverrides={},preferences={},order=[],nextID=1,settings={balanceWarnAt:300,balanceLowAt:100};
const library=()=>({models:customModels,legacy_overrides:legacyOverrides,preferences,order});
const server=http.createServer((req,res)=>{
 const url=new URL(req.url,'http://localhost');
 const json=data=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify(data))};
 const read=callback=>{let body='';req.on('data',chunk=>body+=chunk);req.on('end',()=>callback(JSON.parse(body||'{}')))};
 if(url.pathname==='/models'||url.pathname==='/models/refresh')return json(models);
 if(url.pathname==='/account/models'&&req.method==='POST')return read(input=>{
  const id='custom-'+nextID++;customModels.push({id,name:input.name||'Claude Fable 5.2',provider:input.provider,url:input.url,tier:input.tier||'max',description:input.description,inputs:['text'],context_window:input.context_window,custom:true});legacyOverrides[id]=input.legacy;preferences[id]={hidden:input.hidden};json({...library(),changed_id:id});
 });
 if(url.pathname==='/account/models/order')return read(input=>{order=input.ids;json(library())});
 if(url.pathname==='/account/models/actions')return read(input=>{customModels=customModels.filter(m=>!input.ids.includes(m.id));input.ids.forEach(id=>{if(!id.startsWith('custom-'))preferences[id]={...preferences[id],hidden:true}});json(library())});
 if(url.pathname.startsWith('/account/models/')&&req.method==='PATCH')return read(input=>{const id=url.pathname.split('/').pop();if(input.reset){delete preferences[id];delete legacyOverrides[id]}else{preferences[id]={...preferences[id],...input};if(Object.hasOwn(input,'legacy'))legacyOverrides[id]=input.legacy}json(library())});
 if(url.pathname.startsWith('/account/models/')&&req.method==='DELETE'){const id=url.pathname.split('/').pop();customModels=customModels.filter(m=>m.id!==id);if(!id.startsWith('custom-'))preferences[id]={...preferences[id],hidden:true};return json(library())}
 // Deliberately let settings and the public catalog win this race. A saved
 // personal model must still restore when its private list arrives later.
 if(url.pathname==='/account/models')return setTimeout(()=>json(library()),200);
 if(url.pathname==='/auth/session')return json({signed_in:true,user_id:'test'});
 if(url.pathname==='/account/settings'&&req.method==='PUT')return read(input=>{settings=input;res.statusCode=204;res.end()});
 if(url.pathname==='/account/settings')return json(settings);
 if(url.pathname==='/account/balance')return json({current_point_balance:123456,unit:'points'});
 if(url.pathname==='/conversations')return json({conversations:[]});
 if(url.pathname==='/projects')return json({projects:[]});
 if(url.pathname==='/events'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write(': connected\n\n');return}
 const file=url.pathname==='/'?path.join(root,'server/frontend.html'):path.resolve(root,'server','.'+url.pathname);
 if(!file.startsWith(path.join(root,'server')+path.sep)||!fs.existsSync(file)||!fs.statSync(file).isFile()){res.statusCode=404;return res.end()}
 res.setHeader('Content-Type',({'.html':'text/html','.js':'text/javascript','.css':'text/css','.json':'application/json','.svg':'image/svg+xml','.woff2':'font/woff2','.ttf':'font/ttf'})[path.extname(file)]||'application/octet-stream');res.end(fs.readFileSync(file));
});
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH||undefined,args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1280,height:950}});const errors=[];page.on('pageerror',error=>errors.push(error.message));
  await page.addInitScript(()=>{if(window!==window.top)return;if(!localStorage.getItem('test-initialized')){localStorage.setItem('test-initialized','1');localStorage.setItem('neochat-provider-key','pza_old');localStorage.setItem('neochat-image-key','pza_old_media');localStorage.setItem('neochat-preferences-v1',JSON.stringify({balanceWarnAt:300,balanceLowAt:100}))}});
  const providerHeaders=[];page.on('request',r=>{const h=r.headers();if(h['x-provider-key']||h['x-image-key'])providerHeaders.push(h)});
  const origin='http://127.0.0.1:'+server.address().port;
  await page.goto(origin,{waitUntil:'domcontentloaded'});
  await page.waitForFunction(()=>window.NeoChatModels&&window.NeoChatModels.current());
  await page.waitForFunction(()=>!document.documentElement.classList.contains('is-guest'));
  assert.equal(await page.locator('.model-row[data-model-id="gpt-6-luna"] .model-meta').count(),0);
  assert.equal(await page.locator('.model-row[data-model-id="disabled"]').isDisabled(),true);
  assert.equal(await page.locator('#balanceWarnAt').inputValue(),'10000');assert.equal(await page.locator('#balanceLowAt').inputValue(),'1000');assert.equal(providerHeaders.length,0);
  await page.evaluate(()=>document.getElementById('openModelManager').click());
  await page.locator('#modelManagerDialog').waitFor({state:'visible'});
  await page.locator('#mmNew').click();await page.locator('#customModelProvider').selectOption('anthropic');await page.locator('#customModelName').fill('Fable personal');await page.locator('#customModelDescription').fill('Deep research');await page.locator('#customModelURL').fill('https://poe.com/Claude-Fable-5.2');await page.locator('#customModelTier').selectOption('thinking');await page.locator('#customModelContext').fill('1048576');await page.locator('#mmSave').click();
  await page.waitForFunction(()=>window.NeoChatModels.current().models.some(m=>m.id==='custom-1'));
  assert.equal(customModels[0].context_window,1048576);
  await page.locator('#mmLegacy').check();await page.locator('#mmSave').click();
  await page.waitForFunction(()=>document.querySelector('.model-list[data-legacy] .model-row[data-model-id="custom-1"]'));
  assert.equal(await page.locator('.model-row[data-model-id="custom-1"] .model-meta').textContent(),'Deep research');
  await page.locator('#mmDone').click();
  const savedSelection=page.waitForResponse(response=>response.url().endsWith('/account/settings')&&response.request().method()==='PUT'&&response.request().postDataJSON().manualModelId==='custom-1');
  await page.evaluate(()=>document.querySelector('.model-row[data-model-id="custom-1"]').click());await savedSelection;
  assert.equal(settings.manualModelId,'custom-1','selection was not saved immediately');
  await page.evaluate(()=>localStorage.removeItem('neochat-preferences-v1'));
  await page.reload({waitUntil:'domcontentloaded'});
  await page.waitForFunction(()=>document.querySelector('.model-row[data-model-id="custom-1"]'));
  assert.equal(await page.locator('#manualState').getAttribute('data-model-id'),'custom-1','saved custom selection was dropped while catalog loaded');
  assert.equal(await page.locator('#triggerModelName').textContent(),'Fable personal');
  await page.evaluate(()=>document.getElementById('openModelManager').click());
  await page.locator('#mmProvider').selectOption('openai');await page.getByRole('button',{name:'Move GPT-6 Luna up',exact:true}).click();
  await page.waitForFunction(()=>window.NeoChatModels.current().models[0].id==='gpt-6-luna');
  assert.equal(order[0],'gpt-6-luna');
  await page.locator('.mm-model[data-id="gpt-6-luna"] .mm-model-pick').click();await page.locator('#customModelName').fill('My Luna');await page.locator('#customModelURL').fill('https://poe.com/My-Luna');await page.locator('#customModelDescription').fill('Focused work');await page.locator('#customModelTier').selectOption('max');await page.locator('#mmSave').click();
  await page.waitForFunction(()=>window.NeoChatModels.current().models.find(m=>m.id==='gpt-6-luna').name==='My Luna');
  assert.equal(await page.locator('.model-row[data-model-id="gpt-6-luna"] .model-meta').textContent(),'Focused work');
  await page.locator('#mmNew').click();await page.locator('#customModelName').fill('Mini personal');await page.locator('#customModelURL').fill('https://poe.com/GPT-5.4-mini');await page.locator('#mmSave').click();
  await page.waitForFunction(()=>window.NeoChatModels.current().models.some(m=>m.id==='custom-2'));
  await page.locator('#mmDuplicates').click();await page.locator('input[type="radio"][value="custom-2"]').check();await page.getByRole('button',{name:'Keep selected · remove 1',exact:true}).click();
  await page.waitForFunction(()=>!document.querySelector('.model-row[data-model-id="gpt-5.4-mini"]'));
  assert.equal(preferences['gpt-5.4-mini'].hidden,true);assert.equal(await page.locator('.model-row[data-model-id="custom-2"]').count(),1);
  const raw=await page.evaluate(()=>JSON.parse(localStorage.getItem('neochat-models-poe-v2')));
  assert.ok(!raw.models.some(m=>m.id.startsWith('custom-')),'private custom models leaked into catalog cache');assert.equal(raw.models.find(m=>m.id==='gpt-6-luna').name,'GPT-6 Luna','personal edits leaked into shared cache');
  await page.evaluate(()=>window.NeoChatModels.apply({models:[{id:'fresh',name:'Fresh',provider:'openai',tier:'instant',url:'https://poe.com/Fresh',inputs:['text']}]}));
  assert.equal(await page.locator('.model-row[data-model-id="custom-1"]').count(),1,'catalog refresh dropped personal model');
  await page.locator('#mmDone').click();await page.reload({waitUntil:'domcontentloaded'});await page.waitForFunction(()=>document.querySelector('.model-row[data-model-id="custom-1"]'));
  assert.equal(await page.locator('#triggerModelName').textContent(),'Fable personal');assert.equal(await page.locator('.model-row[data-model-id="gpt-5.4-mini"]').count(),0);assert.equal(await page.locator('.model-row[data-model-id="gpt-6-luna"]').getAttribute('data-model'),'My Luna');
  await page.evaluate(()=>window.NeoChatPreferences.apply(Object.assign({},window.NeoChatPreferences.current(),{theme:'dark'})));
  await page.evaluate(()=>document.getElementById('openModelManager').click());await page.screenshot({path:'/tmp/neochat-model-manager-desktop.png'});
  await page.setViewportSize({width:390,height:844});await page.screenshot({path:'/tmp/neochat-model-manager-mobile.png'});
  const layout=await page.locator('#modelManagerDialog').evaluate(node=>({width:node.getBoundingClientRect().width,left:node.getBoundingClientRect().left,scroll:node.scrollWidth,client:node.clientWidth}));assert.ok(layout.left>=0&&layout.width<=390&&layout.scroll<=layout.client+2,'mobile dialog overflow');
  await page.locator('#mmFilter').selectOption('all');await page.locator('#mmProvider').selectOption('anthropic');await page.locator('.mm-model[data-id="custom-1"] .mm-model-pick').click();await page.locator('#mmRemove').click();await page.waitForFunction(()=>!document.querySelector('.model-row[data-model-id="custom-1"]'));
  assert.notEqual(await page.locator('#manualState').getAttribute('data-model-id'),'custom-1','deleted selection remained active');
  assert.deepEqual(errors,[],'browser errors');
  console.log('Poe UI: model manager add/edit/reorder/Legacy/duplicates/remove, mobile layout, cache isolation and delayed saved-selection restoration passed.');
 }finally{await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve))}
})().catch(error=>{console.error(error);process.exitCode=1});
