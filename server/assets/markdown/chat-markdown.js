/* markdown-it tokens rendered through NeoChat's existing DOM components. */
(function(global){
  'use strict';
  function create(hooks){
    const md=global.markdownit({html:false,linkify:true,typographer:false,breaks:false});
    // Keep the existing KaTeX components; texmath only tokenizes delimiters.
    md.use(global.texmath,{delimiters:['dollars','brackets'],engine:{}});
    md.block.ruler.before('html_block','neochat_image',function(state,startLine,endLine,silent){
      const line=state.src.slice(state.bMarks[startLine]+state.tShift[startLine],state.eMarks[startLine]).trim();
      if(!line.startsWith('<!--neochat-image:'))return false;
      if(!silent){
        const token=state.push('neochat_image','',0);
        token.meta=hooks.parseInlineImage(line);
        token.map=[startLine,startLine+1];
        state.line=startLine+1;
      }
      return true;
    });
    const tags=new Set(['p','h1','h2','h3','h4','h5','h6','blockquote','ul','ol','li','strong','em','s','a','table','thead','tbody','tr','th','td']);
    function imageAlt(tokens){
      return (tokens||[]).map(function(token){return token.children?imageAlt(token.children):token.type==='softbreak'?' ':token.content||''}).join('');
    }
    function fenceComplete(token,source){
      if(!token.map)return true;
      const lines=source.split('\n');
      const last=(lines[token.map[1]-1]||'').trim();
      const fence=token.markup||'```';
      return token.map[1]-1>token.map[0]&&last.length>=fence.length&&Array.from(last).every(function(ch){return ch===fence[0]});
    }
    function renderTokens(tokens,parent,environment){
      const stack=[parent];
      tokens.forEach(function(token){
        const current=stack[stack.length-1];
        if(token.nesting===-1){if(stack.length>1)stack.pop();return}
        if(token.type==='inline'){renderTokens(token.children||[],current,environment);return}
        if(token.type==='neochat_image'){
          if(token.meta){
            const id=token.meta.display_id;
            const previous=environment.images.get(id);
            environment.images.delete(id);
            current.append(previous||hooks.inlineImageElement(token.meta));
          }
          return;
        }
        if(token.type.startsWith('math_')){
          current.append(hooks.mathElement(token.content,token.block===true,true));return;
        }
        if(token.type==='fence'||token.type==='code_block'){
          const language=(token.info||'').trim().split(/\s+/)[0];
          current.append(hooks.codeBlock(language,token.content.replace(/\n$/,''),token.type==='code_block'||fenceComplete(token,environment.source)));return;
        }
        if(token.type==='code_inline'){
          const code=document.createElement('code');code.textContent=token.content;current.append(code);return;
        }
        if(token.type==='softbreak'){current.append(document.createTextNode('\n'));return}
        if(token.type==='hardbreak'||token.type==='hr'){current.append(document.createElement(token.type==='hr'?'hr':'br'));return}
        if(token.type==='image'){
          const image=document.createElement('img');
          const src=token.attrGet('src')||'';
          // markdown-it validates protocols, including data-image types.
          if(md.validateLink(src))image.src=src;
          image.alt=imageAlt(token.children);
          const title=token.attrGet('title');if(title)image.title=title;
          current.append(image);return;
        }
        if(token.nesting===1){
          if(token.hidden||!tags.has(token.tag)){stack.push(current);return}
          const node=document.createElement(token.tag==='s'?'del':token.tag);
          if(token.tag==='a'){
            const href=token.attrGet('href')||'';
            if(md.validateLink(href))node.setAttribute('href',href);
            const title=token.attrGet('title');if(title)node.title=title;
            node.target='_blank';node.rel='noopener noreferrer';
          }
          if(token.tag==='ol'){
            const start=token.attrGet('start');if(start)node.setAttribute('start',start);
          }
          if(token.tag==='th'||token.tag==='td'){
            if(token.tag==='th')node.scope='col';
            const align=/^text-align:(left|center|right)$/.exec(token.attrGet('style')||'');
            if(align&&align[1]!=='left')node.className='align-'+align[1];
          }
          if(token.tag==='table'){
            const wrapper=document.createElement('div');wrapper.className='chat-table-wrap';wrapper.append(node);current.append(wrapper);
          }else current.append(node);
          stack.push(node);return;
        }
        if(token.content)current.append(document.createTextNode(token.content));
      });
    }
    function render(parent,source){
      source=String(source||'').replace(/\r\n?/g,'\n');
      const images=new Map(Array.from(parent.querySelectorAll('.inline-answer-image')).map(function(node){return [node.dataset.displayId,node]}));
      const fragment=document.createDocumentFragment();
      renderTokens(md.parse(source,{}),fragment,{source:source,images:images});
      parent.replaceChildren(fragment);
    }
    return {render:render};
  }
  global.NeoChatMarkdown={create:create};
})(window);
