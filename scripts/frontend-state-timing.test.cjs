const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const html=fs.readFileSync(require('node:path').join(__dirname,'../server/frontend.html'),'utf8');
function section(from,to){return html.slice(html.indexOf(from),html.indexOf(to,html.indexOf(from)))}
function harness(events=[]){
  let now=0, nextTimer=1;
  const timers=new Map(), delivered=[], frames=[];
  const controller=new AbortController();
  const context=vm.createContext({
    performance:{now:()=>now},Date,JSON,Array,Math,Boolean,String,Number,Promise,
    AbortController,DOMException,TextDecoder,
    setTimeout:(fn,ms)=>{const id=nextTimer++;timers.set(id,{fn,at:now+ms});return id},
    clearTimeout:id=>timers.delete(id),setInterval:()=>0,clearInterval:()=>{},
    document:{addEventListener(){},removeEventListener(){}},
    pause:async(ms,signal)=>{if(signal.aborted)throw new DOMException('Aborted','AbortError');now+=ms},
    apiFetch:async()=>new Response(events.map(([event,data])=>'event: '+event+'\ndata: '+JSON.stringify(data)+'\n\n').join('')),
    STREAM_STALL_MS:45000,STREAM_RETRIES:5,STREAM_RETRY_DELAYS:[],
    setStreamStatus(){},whenVisible:async()=>{},streamFailure:(text,details)=>Object.assign(new Error(text),details),
    renderThought:(message,state)=>frames.push(JSON.parse(JSON.stringify(state)))
  });
  vm.runInContext(section('    function parseServerSentEvent(', '    /* ---------- resumable streams'),context);
  vm.runInContext(section('    async function runChatStream(', '    // The note that lets'),context);
  vm.runInContext(section('    function thoughtTracker(', '    /* ---------- questionnaires'),context);
  return {context,controller,delivered,frames,
    advance(ms){now+=ms;for(const [id,timer] of timers)if(timer.at<=now){timers.delete(id);timer.fn()}},
    run(onEvent){
      return context.runChatStream({signal:controller.signal,request:{},onEvent:(event,data)=>{
        delivered.push({event,data,at:now});
        if(onEvent)onEvent(event,data);
      }});
    }
  };
}
const activity=(tool,phase,pending=true)=>['activity',{activity:[{tool,phase,pending}]}];
test('writing, execution, file sharing and following text each respect the state minimum',async()=>{
  const h=harness([activity('run_script','writing'),activity('run_script','running'),activity('run_script','running',false),['delta',{text:'Intermediate'}],activity('send_file',''),activity('send_file','',false),['delta',{text:'Final'}],['done',{response_text:'IntermediateFinal'}]]);
  await h.run();
  assert.deepEqual(h.delivered.map(e=>e.at),[0,1200,2400,2400,2400,3600,3600,3600]);
  assert.deepEqual(h.delivered.filter(e=>e.event==='delta').map(e=>e.data.text),['Intermediate','Final']);
});
test('updates to a writing script do not restart its minimum',async()=>{
  const h=harness([activity('run_script','writing'),['activity',{activity:[{tool:'run_script',phase:'writing',pending:true,purpose:'Calculate'}]}],activity('run_script','running'),['done',{}]]);
  await h.run();
  assert.deepEqual(h.delivered.map(e=>e.at),[0,0,1200,2400]);
});
test('tool state stays paired through reasoning, returns to thinking after 700ms, and text cancels the timer',()=>{
  const h=harness(), message={querySelector:()=>null};
  const tracker=h.context.thoughtTracker(message,0);
  tracker.activity([{tool:'run_script',phase:'writing',pending:true}]);
  tracker.activity([{tool:'run_script',pending:false}]);
  tracker.reasoning('Next step');
  h.advance(699);
  assert.equal(h.frames.at(-1).displayRunning[0].phase,'writing');
  h.advance(1);
  assert.equal(h.frames.at(-1).displayRunning.length,0);
  tracker.activity([{tool:'send_file',pending:true}]);
  tracker.activity([{tool:'send_file',pending:false}]);
  tracker.text();
  const count=h.frames.length;
  h.advance(1000);
  assert.equal(h.frames.length,count);
  assert.equal(h.frames.at(-1).live,false);
});
test('stop clears the idle timer and abort interrupts a pending transition',async()=>{
  const h=harness([activity('run_command',''),activity('run_command','',false),['done',{}]]);
  await assert.rejects(h.run(event=>{if(event==='activity')h.controller.abort()}),{name:'AbortError'});
  assert.equal(h.delivered.length,1);
  const tracker=h.context.thoughtTracker({querySelector:()=>null},0);
  tracker.activity([{tool:'run_command',pending:true}]);
  tracker.activity([{tool:'run_command',pending:false}]);
  tracker.stop();
  const count=h.frames.length;
  h.advance(1000);
  assert.equal(h.frames.length,count);
  assert.equal(h.frames.at(-1).live,false);
});
test('a long-running operation adds no delay and a reply without deltas still waits',async()=>{
  const h=harness([activity('web_search',''),activity('web_search','',false),['done',{}]]);
  await h.run((event,data)=>{if(event==='activity'&&data.activity[0].pending)h.advance(2000)});
  assert.deepEqual(h.delivered.map(e=>e.at),[0,2000,2000]);
  const short=harness([activity('send_file',''),['done',{response_text:'File ready'}]]);
  await short.run();
  assert.equal(short.delivered.at(-1).at,1200);
});
test('a new action cancels the return to thinking, for all tool types',()=>{
  for(const tool of ['run_command','run_script','send_file','read_file','write_file','list_dir','mcp','web_search','web_fetch']){
    const h=harness(),tracker=h.context.thoughtTracker({querySelector:()=>null},0);
    tracker.activity([{tool:'run_command',pending:true}]);
    tracker.activity([{tool:'run_command',pending:false}]);
    h.advance(600);
    tracker.activity([{tool,pending:true}]);
    h.advance(200);
    tracker.reasoning('Still working');
    assert.equal(h.frames.at(-1).displayRunning[0].tool,tool);
    tracker.done({});
    const count=h.frames.length;
    h.advance(1000);
    assert.equal(h.frames.length,count);
  }
});

function smoothStreamHarness(){
  let now=0,nextID=1;const timers=new Map(),frames=[];
  const body={textContent:''},message={dataset:{},querySelector:()=>body};
  const context=vm.createContext({performance:{now:()=>now},Math,Array,String,Promise,
    chatLog:{scrollHeight:0,scrollTop:0,clientHeight:0},STREAM_FADE_MS:240,
    streamTextParts:text=>Array.from(text),renderMarkdown:(node,text)=>{node.textContent=text;frames.push(text)},fadeNewText(){},
    setTimeout:(fn,ms)=>{const id=nextID++;timers.set(id,{fn,at:now+ms});return id},clearTimeout:id=>timers.delete(id)
  });
  vm.runInContext(section('    function createSmoothMessageStream(', '    async function regenerateMessage('),context);
  return {stream:context.createSmoothMessageStream(message),frames,
    advance(ms){now+=ms;for(const [id,timer]of timers)if(timer.at<=now){timers.delete(id);timer.fn()}},
    drain(){for(let i=0;timers.size&&i<10000;i++)this.advance(24);assert.equal(timers.size,0)}
  };
}
test('a complete answer after long thinking still renders progressively',async()=>{
  const h=smoothStreamHarness();h.advance(30000);const text='Ответ '.repeat(40);
  h.stream.append(text);h.stream.replace(text);const done=h.stream.finish();h.advance(24);
  assert.ok(h.frames[0].length>0&&h.frames[0].length<text.length,'first tick dumped the complete answer');
  h.drain();await done;assert.equal(h.stream.visibleText(),text);assert.ok(h.frames.length>3);
});
test('text after a pause and a stalled browser frame never catches up in one burst',()=>{
  const h=smoothStreamHarness();h.stream.append('Начало.');h.drain();h.advance(20000);
  const before=h.stream.visibleText().length;h.stream.append(' Продолжение.'.repeat(40));h.advance(24);
  assert.ok(h.stream.visibleText().length-before<=48);
  const length=h.stream.visibleText().length;h.advance(10000);
  assert.ok(h.stream.visibleText().length-length<=48,'stalled frame dumped the backlog');
  h.stream.cancel();
});
test('whole-response replacement preserves final text and cancellation stops painting',async()=>{
  const h=smoothStreamHarness();h.stream.append('Old text');h.drain();const text='New answer '.repeat(40);
  h.stream.replace(text);const done=h.stream.finish();h.advance(24);
  assert.ok(h.frames.at(-1).length<text.length);h.drain();await done;assert.equal(h.stream.visibleText(),text);
  const stop=smoothStreamHarness();stop.stream.append(text);stop.advance(24);stop.stream.cancel();const count=stop.frames.length;stop.advance(10000);assert.equal(stop.frames.length,count);
});
