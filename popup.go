// Chrome draws <select> dropdowns as native OS popup windows. CDP screencast
// and CaptureScreenshot only ever see the page, so clicking one in WRP focuses
// the element and nothing appears - the same reason the right click menu had to
// be drawn by hand. This swaps the native popup for a listbox built inside the
// page, which then rides along in the rendered frame and works in VNC and ISMAP
// alike. Installed per tab, at document start, so it covers subframes too.
package main

import (
	"context"
	"log"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Runs in the page, once per document. Idempotent, because a tab can be handed
// the script twice - once for the document it already has, once at the start of
// the next one.
const selPopupJS = `(function(){
if(window.__wrpSel)return;
window.__wrpSel={box:function(){return box},sel:function(){return sel},
 row:function(i){return items[i]}};
// the room the select has bounds the list long before MAXH does, except under
// an ISMAP capture, which stretches the viewport to the whole page: there the
// list is drawn in full rather than stuck behind a scrollbar nothing can reach
var MAXH=2000,box=null,sel=null,items=[],hi=-1;
function kill(){if(box&&box.parentNode)box.parentNode.removeChild(box);box=null;sel=null;items=[];hi=-1}
function paint(){for(var i=0;i<items.length;i++){if(!items[i])continue;var on=i===hi;
 items[i].style.background=on?'#1a73e8':'transparent';
 items[i].style.color=on?'#fff':(sel.options[i].disabled?'#999':'#000')}}
function pick(i){var s=sel;
 if(!s||i<0||i>=s.options.length||s.options[i].disabled)return;
 var ch=s.selectedIndex!==i;kill();
 if(!ch)return;
 s.selectedIndex=i;
 s.dispatchEvent(new Event('input',{bubbles:true}));
 s.dispatchEvent(new Event('change',{bubbles:true}))}
// Position under the select. Nothing here is assumed about the coordinate space
// the box lives in: the containing block could be the viewport, a positioned
// ancestor or a transformed one, and WRP's own page zoom scales every length we
// set, so the offset from setting left/top is measured rather than reasoned
// about. Runs again on resize - an ISMAP capture stretches the viewport to the
// whole page between the click and the screenshot, and a list that closed
// itself there would never make it into the picture.
function place(){
 if(!box||!sel)return;
 var w=sel.ownerDocument.defaultView,r=sel.getBoundingClientRect(),st=box.style;
 st.left='0px';st.top='0px';
 var g0=box.getBoundingClientRect();
 st.left='100px';
 var kx=(box.getBoundingClientRect().left-g0.left)/100||1;
 st.left='0px';st.top='100px';
 var ky=(box.getBoundingClientRect().top-g0.top)/100||1;
 st.top='0px';
 st.minWidth=(Math.max(r.width,80)/kx)+'px';st.maxWidth=(w.innerWidth*0.95/kx)+'px';
 st.maxHeight=(Math.min(MAXH,Math.max(64,Math.max(w.innerHeight-r.bottom,r.top)-6))/ky)+'px';
 var g=box.getBoundingClientRect(),h=g.height,bw=g.width,y=r.bottom;
 if(y+h>w.innerHeight)y=r.top-h>=0?r.top-h:Math.max(0,w.innerHeight-h);
 var x=Math.max(0,Math.min(r.left,w.innerWidth-bw-2));
 st.left=((x-g.left)/kx)+'px';st.top=((y-g.top)/ky)+'px'}
function open(s){
 kill();sel=s;
 var d=s.ownerDocument,b=d.body||d.documentElement,cs=d.defaultView.getComputedStyle(s);
 box=d.createElement('div');
 var st=box.style;
 st.position='absolute';st.left='0px';st.top='0px';st.zIndex='2147483647';
 st.background='#fff';st.color='#000';st.border='1px solid #767676';
 st.boxShadow='0 2px 6px rgba(0,0,0,.35)';st.borderRadius='0';
 st.fontFamily=cs.fontFamily;st.fontSize=cs.fontSize;
 st.fontWeight='normal';st.fontStyle='normal';st.textAlign='left';
 st.lineHeight='normal';st.padding='1px 0';st.margin='0';
 st.overflowY='auto';st.overflowX='hidden';st.boxSizing='border-box';
 // walk options and group labels in document order: a native dropdown shows
 // the labels, and a grouped list reads as one flat run without them. Labels
 // are rows in the box but never entries in items, so they cannot be hovered,
 // keyboard selected or picked.
 var nodes=s.querySelectorAll('optgroup,option');
 for(var k=0;k<nodes.length;k++)(function(n){
  var e=d.createElement('div'),es=e.style;
  es.padding='2px 8px';es.whiteSpace='nowrap';es.cursor='default';
  if(n.tagName==='OPTGROUP'){
   e.textContent=(n.label||'').replace(/\s+/g,' ').trim()||' ';
   es.fontWeight='bold';es.color='#555';es.background='#f0f0f0';
   box.appendChild(e);return}
  var i=n.index;
  e.textContent=(n.textContent||'').replace(/\s+/g,' ').trim()||' ';
  if(n.parentNode&&n.parentNode.tagName==='OPTGROUP')es.paddingLeft='20px';
  e.onmouseover=function(){hi=i;paint()};
  e.onmousedown=function(ev){ev.preventDefault();ev.stopPropagation()};
  e.onclick=function(ev){ev.preventDefault();ev.stopPropagation();pick(i)};
  items[i]=e;box.appendChild(e)})(nodes[k]);
 hi=s.selectedIndex;paint();
 b.appendChild(box);place();
 if(items[hi])box.scrollTop=Math.max(0,items[hi].offsetTop-box.clientHeight/2)}
function hit(ev){
 var p=ev.composedPath?ev.composedPath():null,t=p&&p.length?p[0]:ev.target;
 return t&&t.closest?t.closest('select'):null}
function usable(s){return s&&!s.multiple&&!s.disabled&&s.size<=1&&s.options.length>0}
document.addEventListener('mousedown',function(ev){
 if(box&&box.contains(ev.target))return;
 var s=hit(ev);
 if(!usable(s)){kill();return}
 ev.preventDefault(); // this is what stops the native popup from ever opening
 var same=sel===s;kill();
 if(same)return;
 try{s.focus()}catch(e){}
 open(s)},true);
document.addEventListener('keydown',function(ev){
 if(box){
  if(ev.key==='Escape'){ev.preventDefault();kill();return}
  if(ev.key==='Enter'||ev.key==='Tab'){ev.preventDefault();pick(hi);return}
  if(ev.key!=='ArrowDown'&&ev.key!=='ArrowUp')return;
  ev.preventDefault();
  hi=Math.max(0,Math.min(items.length-1,hi+(ev.key==='ArrowDown'?1:-1)));
  paint();if(items[hi])items[hi].scrollIntoView({block:'nearest'});return}
 var a=document.activeElement;
 if(!usable(a)||a.tagName!=='SELECT')return;
 if(ev.key===' '||(ev.altKey&&(ev.key==='ArrowDown'||ev.key==='ArrowUp'))){
  ev.preventDefault();open(a)}},true);
window.addEventListener('resize',place,true);
})()`

// Install on the tab behind ctx: on every document it loads from now on, and on
// the one it already has.
func selPopupInstall(ctx context.Context) {
	if ctx == nil {
		return
	}
	err := vncRun(ctx, vncTabTO, chromedp.ActionFunc(func(cx context.Context) error {
		if _, err := page.AddScriptToEvaluateOnNewDocument(selPopupJS).Do(cx); err != nil {
			return err
		}
		return chromedp.Evaluate(selPopupJS, nil).Do(cx)
	}))
	if err != nil {
		log.Printf("select dropdown support unavailable on this tab: %v", err)
	}
}
