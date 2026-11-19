const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const path=require('node:path');
const html=fs.readFileSync(path.join(__dirname,'../server/frontend.html'),'utf8');
const start=html.indexOf('    function parseInlineImage(');
const end=html.indexOf('    const chatMarkdown=',start);
const context=vm.createContext({TextDecoder,Uint8Array,atob,JSON,Array,String});
vm.runInContext(html.slice(start,end),context);
const shown={display_id:'a'.repeat(32),file:{id:'b'.repeat(32),name:'diagram.png',kind:'image',mime:'image/png',size:100},caption:'Схема: вход → выход'};
function marker(data){return '<!--neochat-image:'+Buffer.from(JSON.stringify(data)).toString('base64url')+'-->'}
test('inline image payload preserves Unicode captions and file metadata',()=>{
  const result=context.parseInlineImage(marker(shown));
  assert.equal(result.caption,shown.caption);
  assert.equal(result.file.id,shown.file.id);
  assert.equal(context.parseInlineImage('<!--neochat-image:broken-->'),null);
});
test('reject HTML, external URLs and invalid payloads before creating image nodes',()=>{
  for(const data of [null,{}, {...shown,file:{...shown.file,kind:'text',mime:'text/html'}},{...shown,file:{...shown.file,id:'https://evil.example/image.png'}},{...shown,caption:{html:'<script>'}},{...shown,display_id:'bad'}]){
    assert.equal(context.parseInlineImage(marker(data)),null);
  }
});
test('streaming treats the picture as one unit between introductory and following text',()=>{
  const image=marker(shown),text='Before.\n\n'+image+'\n\nAfter.';
  const parts=Array.from(context.streamTextParts(text));
  assert.equal(parts.join(''),text);
  assert.equal(parts.filter(part=>part===image).length,1);
  assert.ok(parts.indexOf(image)>0);
  assert.ok(parts.indexOf(image)<parts.length-1);
  assert.equal(context.readableInlineImages(text),'Before.\n\n[Image: '+shown.caption+']\n\nAfter.');
});
