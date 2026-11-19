const path=require('node:path');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||path.join(__dirname,'../web/markdown-tests/node_modules/playwright'));
const fs=require('fs'),assert=require('assert/strict');
const root=path.resolve(__dirname,'..')+'/';const html=fs.readFileSync(root+'server/frontend.html','utf8');
function section(from,to){const i=html.indexOf(from);assert.ok(i>=0,from);const j=html.indexOf(to,i);assert.ok(j>i,to);return html.slice(i,j)}
(async()=>{
 const browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH||undefined,headless:true,args:['--no-sandbox']});
 try{
 const page=await browser.newPage({viewport:{width:1100,height:1000}});const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.setContent('<!doctype html><html data-theme="light"><body><div class="chat-message assistant"><div class="chat-copy"><div class="chat-text" id="output"></div></div></div></body></html>');
 await page.addStyleTag({content:fs.readFileSync(root+'server/assets/fluid.css','utf8')+'body{display:block;padding:32px} .chat-message{max-width:800px;margin:auto}'});
 for(const name of ['markdown/markdown-it.min.js','markdown/texmath.js','markdown/chat-markdown.js','katex/katex.min.js','highlight/highlight.min.js'])await page.addScriptTag({path:root+'server/assets/'+name});
 await page.addScriptTag({content:`function copyMessageText(text){window.copied=text;return Promise.resolve()};function toast(){};function openArtifact(text){window.openedArtifact=text};function openAttachment(file){window.openedImage=file.id};function downloadSentFile(){};function attachmentURL(){return Promise.resolve('data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jcS8AAAAASUVORK5CYII=')};`+
 section('    /* ---------- math ---------- */','    /* ---------- artifacts ---------- */')+
 section('    function isArtifactSource(','    function openArtifact(')+
 section('    const codeCopyIcon=','    function parseInlineImage(')+
 section('    function parseInlineImage(','    // While a model thinks,')});
 async function render(source){await page.evaluate(source=>renderMarkdown(document.getElementById('output'),source),source)}
 const url='https://github.com/p1neappleXpress/OpenFlux';
 await render(`**[OpenFlux](${url})** and [**OpenFlux**](${url} "Repository")`);
 assert.equal(await page.locator('#output strong > a').textContent(),'OpenFlux');
 assert.equal(await page.locator('#output a > strong').textContent(),'OpenFlux');
 assert.equal(await page.locator('#output a').last().getAttribute('title'),'Repository');
 await render('[Repository][repo]\n\n[repo]: '+url+' "Reference"\n\n<https://example.com> and https://example.org/path_(one)');
 assert.equal(await page.locator('#output a').count(),3);
 assert.equal(await page.locator('#output a').first().getAttribute('href'),url);
 await render('1. Outer\n   - **Nested**\n     > Quoted *text*\n2. Next\n\n---\n\n~~removed~~ and `literal *code*`');
 assert.equal(await page.locator('#output ol > li > ul > li strong').textContent(),'Nested');
 assert.equal(await page.locator('#output blockquote em').textContent(),'text');
 assert.equal(await page.locator('#output del').textContent(),'removed');
 await render('| Item | Value |\n| :--- | ---: |\n| [Link]('+url+') | **42** |');
 assert.equal(await page.locator('#output .chat-table-wrap > table').count(),1);
 assert.equal(await page.locator('#output td.align-right strong').textContent(),'42');
 const tableStyle=await page.locator('#output td.align-right').evaluate(n=>({align:getComputedStyle(n).textAlign,font:getComputedStyle(n).fontFamily}));assert.equal(tableStyle.align,'right');
 await render('```python\nprint("hello")\n```');
 assert.equal(await page.locator('#output .code-block .code-head .code-copy').count(),1);
 assert.equal(await page.locator('#output .code-block pre code').textContent(),'print("hello")');
 await page.locator('.code-copy').click();assert.equal(await page.evaluate(()=>copied),'print("hello")');
 assert.ok((await page.locator('.code-block code').getAttribute('class')).includes('hljs'));
 await render('~~~~html\n<!doctype html><title>Preview</title>\n<p>Hello</p>');
 assert.equal(await page.locator('.artifact-card.is-writing').count(),1);assert.equal(await page.locator('.artifact-card').isDisabled(),true);
 await render('~~~~html\n<!doctype html><title>Preview</title>\n<p>Hello</p>\n~~~~');
 assert.equal(await page.locator('.artifact-card strong').textContent(),'Preview');await page.locator('.artifact-card').click();assert.ok((await page.evaluate(()=>openedArtifact)).includes('<title>Preview</title>'));
 await render('Inline $x^2$ and \\(a+b\\).\n\n$$\n\\frac{a}{b}\n$$\n\n\\[c=d\\]');
 assert.equal(await page.locator('.math-inline .katex').count(),2);assert.equal(await page.locator('.math-block .katex').count(),2);
 // No executable HTML or unsafe links become DOM, including inside tables.
 await render('<script>window.pwned=1</script>\n\n<img src=x onerror="window.pwned=1">\n\n[x](javascript:alert(1))\n\n[x](data:text/html,evil)');
 assert.equal(await page.locator('#output script,#output [onerror],#output a[href^="javascript:"],#output a[href^="data:"]').count(),0);assert.equal(await page.evaluate(()=>window.pwned),undefined);
 const shown={display_id:'a'.repeat(32),file:{id:'b'.repeat(32),name:'diagram.png',kind:'image',mime:'image/png'},caption:'Схема'};
 const marker='<!--neochat-image:'+Buffer.from(JSON.stringify(shown)).toString('base64url')+'-->';
 await render('Before.\n\n'+marker+'\n\nAfter.');assert.deepEqual(await page.locator('#output > *').evaluateAll(nodes=>nodes.map(n=>n.tagName)),['P','FIGURE','P']);
 await page.evaluate(()=>window.previousFigure=document.querySelector('figure'));await render('Before.\n\n'+marker+'\n\nAfter. More text.');assert.equal(await page.evaluate(()=>previousFigure===document.querySelector('figure')),true);
 await render('# Markdown\n\n**[OpenFlux]('+url+')** — ссылка внутри выделения.\n\n| Item | Value |\n| :--- | ---: |\n| [Link]('+url+') | **42** |\n\n```python\nprint("hello")\n```\n\n1. Outer\n   - Nested *item*\n\nInline $x^2$.');
 await page.screenshot({path:process.env.MARKDOWN_SCREENSHOT||path.join(require('node:os').tmpdir(),'neochat-markdown-preview.png')});
 await page.setViewportSize({width:390,height:844});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
 assert.deepEqual(errors,[]);console.log('Passed: nested/reference/autolinks, nested lists/quotes, table wrapper/alignment, original code component/highlight/copy, streaming artifacts, all math delimiters, safe HTML/URLs, inline pictures, mobile layout.');
 }finally{await browser.close()}
})().catch(error=>{console.error(error);process.exitCode=1});
