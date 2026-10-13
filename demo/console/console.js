"use strict";
const $=id=>document.getElementById(id);
const esc=s=>String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;");
const hhmm=ts=>new Date(ts*1000).toLocaleTimeString([],{hour:"2-digit",minute:"2-digit"});
const hhmmss=ts=>new Date(ts*1000).toLocaleTimeString([],{hour:"2-digit",minute:"2-digit",second:"2-digit"});

/* ---- Slack-style markdown ---- */
function md(s){
  const keep=[];const hold=h=>{keep.push(h);return "\u0000"+(keep.length-1)+"\u0000"};
  s=esc(s).replace(/```[a-z]*\n?([\s\S]*?)```/g,(_,c)=>hold("<code>"+c+"</code>"))
    .replace(/`([^`\n]+)`/g,(_,c)=>hold("<code>"+c+"</code>"))
    .replace(/&lt;(https?:\/\/[^|&\s]+)\|([^&]+?)&gt;/g,(_,u,t)=>hold('<a href="'+u+'" target="_blank" rel="noopener">'+t+"</a>"))
    .replace(/\bhttps?:\/\/[^\s<)]+/g,u=>hold('<a href="'+u+'" target="_blank" rel="noopener">'+u+"</a>"));
  s=s.replace(/\*\*([^*\n]+)\*\*|\*([^*\n]+)\*/g,(_,a,b)=>"<b>"+(a||b)+"</b>")
     .replace(/(^|[\s(])_([^_\n]+)_(?=[\s).,]|$)/g,"$1<i>$2</i>")
     .replace(/^&gt; ?(.*)$/gm,"<blockquote>$1</blockquote>").replace(/<\/blockquote>\n/g,"</blockquote>");
  return s.replace(/\u0000(\d+)\u0000/g,(_,i)=>keep[i]);
}

/* ---- chat ---- */
const cards=new Map(),seenMsg=new Set(),allMsgs=[];let replyTo="";
function groupKey(m){
  if(m.thread)return m.thread;
  let x;
  if(x=m.text.match(/\bINC-[A-Z0-9]{5}\b/))return "incident "+x[0];
  if(x=m.text.match(/\bPR\s*#?(\d+)|pull request\s*#?(\d+)|#(\d+)/i))return "PR #"+(x[1]||x[2]||x[3]);
  if(x=m.text.match(/\b(?:approve|reject)\s+([A-Z0-9]{5})\b/i)){
    const c=[...allMsgs].reverse().find(o=>o!==m&&o.text.includes(x[1])&&o.k);if(c)return c.k}
  return "general";
}
function label(m){
  let x;
  if(x=m.text.match(/\bINC-[A-Z0-9]{5}\b/))return "Incident "+x[0];
  if(x=m.text.match(/([\w-]+)#(\d+)/))return "PR "+x[1]+"#"+x[2];
  if(x=m.text.match(/\bPR\s*#?(\d+)/i))return "PR #"+x[1];
  return null;
}
function addMsg(m){
  m.k=groupKey(m);allMsgs.push(m);
  let c=cards.get(m.k);
  const feed=$("feed"),stick=feed.scrollHeight-feed.scrollTop-feed.clientHeight<80;
  if(!c){
    const el=document.createElement("div");el.className="card";if(m.thread)el.dataset.thread=m.thread;
    el.innerHTML="<h4></h4><div class=body></div>";
    el.querySelector("h4").textContent=m.k==="general"?"General":(label(m)||(m.thread?"Thread":m.k));
    el.onclick=()=>setReply(m.k==="general"?"":m.k);
    c={el,body:el.querySelector(".body")};cards.set(m.k,c);
  }
  const h=c.el.querySelector("h4");
  if(m.k!=="general"&&/^(Thread|PR #\d+)$/.test(h.textContent)){const l=label(m);if(l)h.textContent=l}
  const d=document.createElement("div"),me=m.from!=="karmax";
  d.className="msg"+(me?" me":"");
  d.innerHTML='<span class=who>'+esc(me?m.from:"KARMAX")+'<span class=t>'+hhmm(m.ts)+"</span></span>"+md(m.text);
  c.body.appendChild(d);
  feed.appendChild(c.el); // most recent activity at the bottom
  if(stick||me)feed.scrollTop=feed.scrollHeight;
}
function setReply(k){
  replyTo=k;
  document.querySelectorAll(".card").forEach(e=>e.classList.remove("sel"));
  const c=cards.get(k||"general");if(k&&c)c.el.classList.add("sel");
  const lab=k&&c?c.el.querySelector("h4").textContent:"";
  $("target").innerHTML=k?"Replying in <b>"+esc(lab)+"</b> · <a id=x>clear</a>":"";
  if(k)$("x").onclick=e=>{e.stopPropagation();setReply("")};
  $("text").focus();
}
$("f").onsubmit=async e=>{
  e.preventDefault();
  const t=$("text"),text=t.value.trim();if(!text)return;
  const body={text,user:$("user").value||"Maya"};
  const rc=cards.get(replyTo);if(rc&&rc.el.dataset.thread)body.thread=rc.el.dataset.thread;
  $("err").textContent="";t.value="";
  try{
    const r=await fetch("/send",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)});
    const j=await r.json();if(!r.ok)$("err").textContent="⚠ "+(j.error||r.status);
  }catch(x){$("err").textContent="⚠ chat server unreachable: "+x}
  poll();
};

/* ---- derived views ---- */
function incidentFrom(msgs){
  let at=-1;
  for(let i=msgs.length-1;i>=0;i--)if(msgs[i].from==="karmax"&&/🚨 \*?Incident INC-/.test(msgs[i].text)){at=i;break}
  if(at<0)return null;
  const head=msgs[at],id=head.text.match(/INC-[A-Z0-9]{5}/)[0];
  const inc={id,title:(head.text.match(/INC-[A-Z0-9]{5}\*?\s*\[[^\]]*\]\s*([^\n]*)/)||[])[1]||"",sev:(head.text.match(/\[([A-Z]+)\]/)||[])[1]||"",
    s:{detected:{ts:head.ts}},failed:null,pr:null};
  const rest=msgs.slice(at+1),mine=m=>m.from==="karmax";
  for(const m of rest){
    const t=m.text;
    if(mine(m)&&t.includes(id)){
      if(/🟡 \*?Approval needed/.test(t)&&/fix INC/.test(t)&&!inc.s.alerted)inc.s.alerted={ts:m.ts,who:"waiting for approval"};
      if(/👍 Approved by/.test(t)&&!inc.s.approved)inc.s.approved={ts:m.ts,who:(t.match(/Approved by ([^:]+):/)||[])[1]};
      if(/🛠️ On it/.test(t)&&!inc.s.approved)inc.s.approved={ts:m.ts,who:(t.match(/asked by ([^)]+)\)/)||[])[1]};
      if(/🛠️ Sending an engineer/.test(t)&&!inc.s.approved)inc.s.approved={ts:m.ts,who:"policy: auto"};
      let x=t.match(/🔧 Fix for INC-[A-Z0-9]{5} is up: ([\w./-]+)#(\d+)/);
      if(x&&!inc.s.fix){inc.s.fix={ts:m.ts,who:"#"+x[2]};inc.pr={repo:x[1],n:+x[2]}}
      if(/✅ \*?Resolved/.test(t))inc.s.resolved={ts:m.ts};
      if(/❌ The fix attempt|⚠️ The agent finished/.test(t))inc.failed="fix";
    }
    if(mine(m)&&/🛠️ Sending an engineer/.test(t)&&!inc.s.approved)inc.s.approved={ts:m.ts,who:"policy: auto"};
    if(inc.pr&&mine(m)){
      const re=new RegExp("#"+inc.pr.n+"\\b");
      if(/PR review\*?/.test(t)&&re.test(t)&&!inc.s.reviewed){const v=(t.match(/verdict \*([^*]+)\*/)||[])[1];inc.s.reviewed={ts:m.ts,who:v}}
      if(/✅ Merged/.test(t)&&re.test(t)&&!inc.s.merged)inc.s.merged={ts:m.ts};
      if(/🔴 CI failed/.test(t)&&re.test(t))inc.failed="merge";
    }
  }
  if(!inc.s.alerted&&inc.s.approved&&inc.s.approved.who==="policy: auto")inc.s.alerted={ts:head.ts,who:"policy: auto"};
  return inc;
}
function prInfo(st,msgs){
  const out=[];
  const reviews=(st.state&&st.state.reviews)||[];
  for(const p of (st.state&&st.state.prs)||[]){
    const key=p.repo+"#"+p.number,rv=[...reviews].reverse().find(r=>r.repo===p.repo&&r.number===p.number);
    const msg=[...msgs].reverse().find(m=>m.from==="karmax"&&/PR review\*?/.test(m.text)&&m.text.includes(p.repo+"#"+p.number+" "));
    let risk=null,blk=null,warn=null;
    if(msg){let x=msg.text.match(/risk ([\d.]+)/);if(x)risk=+x[1];x=msg.text.match(/(\d+) blocker \/ (\d+) warning/);if(x){blk=+x[1];warn=+x[2]}}
    out.push({key,p,rv,risk,blk,warn,ci:st.ci[key]||"pending",merges:(st.state.merges||[]).includes(key)||p.merged});
  }
  return out.reverse();
}
const verdictBadge=r=>!r.rv?'<span class="badge b-amber">Reviewing…</span>':
  r.rv.event==="REQUEST_CHANGES"?'<span class="badge b-red">⛔ Changes requested</span>':
  r.rv.event==="APPROVE"?'<span class="badge b-green">✅ Approved</span>':'<span class="badge b-acc">💬 Commented</span>';
const ciBadge=s=>s==="success"?'<span class="badge b-green">✓ passing</span>':s==="failure"?'<span class="badge b-red">✗ failing</span>':'<span class="badge b-dim">● pending</span>';

/* ---- renderers ---- */
let lastIncId=null;
function renderProd(h,inc){
  const pts=h.points||[],last=pts[pts.length-1];
  let cls="unk",txt="Unknown";
  if(last){if(last.down){cls="bad";txt="Unreachable"}else if(last.ok){cls="";txt="Healthy"}else{cls="bad";txt="Failing"}}
  const fails=pts.filter(p=>!p.ok).length,rate=pts.length?Math.round(100*fails/pts.length):0;
  const open=inc&&!inc.s.resolved?inc.id:null;
  const maxMs=Math.max(60,...pts.map(p=>p.ms));
  const bars=pts.map(p=>'<i class="'+(p.down?"d":p.ok?"":"f")+'" style="height:'+(p.ok?Math.max(18,Math.round(100*p.ms/maxMs)):100)+'%" title="'+hhmmss(p.t)+" · HTTP "+p.code+'"></i>').join("");
  $("prodbody").innerHTML='<div class="state '+cls+'"><span class=dot></span><span class=big>'+txt+'</span></div>'+
    '<div class=kv><span>HTTP <b>'+(last?(last.code||"—"):"—")+'</b></span><span>Error rate <b style="color:'+(rate?"var(--red)":"inherit")+'">'+rate+'%</b> <small>(2 min)</small></span><span>Latency <b>'+(last?last.ms+" ms":"—")+'</b></span>'+
    (open?'<span>Incident <b class=inc>'+open+'</b></span>':"")+'</div><div class=strip>'+bars+'</div><div class=axis><span>2 min ago</span><span>now</span></div>';
}
const STAGES=[["detected","Detected"],["alerted","Owner alerted"],["approved","Approved"],["fix","Fix PR"],["reviewed","Reviewed"],["merged","Merged"],["resolved","Resolved"]];
function renderInc(inc){
  if(!inc){$("incid").textContent="";$("incbody").innerHTML='<div class=empty>No incident on file — production is quiet.</div>';return}
  $("incid").textContent=inc.id+(inc.sev?" · "+inc.sev.toLowerCase():"");
  let waitIdx=STAGES.findIndex(([k])=>!inc.s[k]);
  const failAt=inc.failed==="fix"?3:inc.failed==="merge"?5:-1;
  const html=STAGES.map(([k,l],i)=>{
    const s=inc.s[k];let cls=s?"done":(i===waitIdx&&!inc.s.resolved?"wait":"");
    if(i===failAt&&!s)cls="fail";
    let who=s?hhmmss(s.ts):"";
    if(s&&s.who)who=(k==="alerted"?"":s.who)+(k==="alerted"?"":" · ")+hhmmss(s.ts);
    if(k==="alerted"&&s&&s.who==="waiting for approval"&&inc.s.approved)who=hhmmss(s.ts);
    if(!s&&cls==="wait")who=k==="approved"?"waiting for a human":"in progress";
    return '<div class="stage '+cls+'"><div class=n>'+(cls==="done"?"✓":cls==="fail"?"!":i+1)+'</div><div class=l>'+l+'</div><div class=w title="'+esc(who)+'">'+esc(who)+'</div></div>';
  }).join("");
  $("incbody").innerHTML='<div class=incsum>'+esc(inc.title)+'</div><div class=track>'+html+'</div>';
}
const known={prs:new Set(),repos:null};
function renderPRs(rows){
  $("prcount").textContent=rows.length?rows.length+" in "+(new Set(rows.map(r=>r.p.repo)).size)+" repo(s)":"";
  if(!rows.length){$("prbody").innerHTML='<div class=empty>No pull requests yet.</div>';return}
  const first=known.prs.size===0&&!window.__prSeen;window.__prSeen=true;
  $("prbody").innerHTML='<table><thead><tr><th>PR</th><th>Title</th><th>Author</th><th>Verdict</th><th>Risk</th><th>Findings</th><th>CI</th><th>Merged</th></tr></thead><tbody>'+
  rows.map(r=>{
    const nw=!first&&!known.prs.has(r.key)?" new":"";
    const f=r.rv?((r.rv.comments||[]).length||r.blk!=null?((r.rv.comments||[]).length):"0"):"—";
    const rk=r.risk==null?"—":'<span class="mono" style="color:'+(r.risk>=.7?"var(--red)":r.risk>=.4?"var(--amber)":"var(--green)")+'">'+r.risk.toFixed(2)+'</span>';
    return '<tr class="row'+nw+'" data-repo="'+esc(r.p.repo)+'" data-n="'+r.p.number+'"><td class=num>#'+r.p.number+'</td><td class=ttl title="'+esc(r.p.title)+'">'+esc(r.p.title)+'</td><td>'+esc(r.p.author)+'</td><td>'+verdictBadge(r)+'</td><td>'+rk+'</td><td class=mono>'+f+(r.blk?' <span style="color:var(--red)">('+r.blk+' blocker)</span>':"")+'</td><td>'+ciBadge(r.ci)+'</td><td>'+(r.merges?'<span class="badge b-green">merged</span>':'<span class="badge b-dim">open</span>')+'</td></tr>';
  }).join("")+"</tbody></table>";
  rows.forEach(r=>known.prs.add(r.key));
  document.querySelectorAll("#prbody tr.row").forEach(tr=>tr.onclick=()=>openPR(tr.dataset.repo,tr.dataset.n));
}
const CK=[["readme","README"],["codeowners","CODEOWNERS"],["claude","CLAUDE.md"],["ci","CI workflow"],["protection","Branch protection"]];
function renderRepos(repos,gh){
  if(!gh.ok&&!repos.length){$("repobody").innerHTML='<div class=empty>GitHub unreachable</div>';return}
  const first=known.repos===null;if(first)known.repos=new Set();
  $("repobody").innerHTML='<table class=rtable><thead><tr><th>Repository</th>'+CK.map(c=>"<th>"+c[1]+"</th>").join("")+"</tr></thead><tbody>"+
  repos.map(r=>{
    const arrive=!first&&!known.repos.has(r.full)?" arrive":"";
    return '<tr class="'+arrive+'"><td><b>'+esc(r.name)+"</b>"+(r.created?'<span class=new-tag>NEW</span>':"")+"</td>"+CK.map(([k])=>{
      if(k==="protection"){const p=r.checks.protection;return p?'<td><span class="tick y">✓</span> <small class=mono style="color:var(--dim)">'+p.reviews+" review"+(p.checks.length?" · "+esc(p.checks.join(",")):"")+"</small></td>":'<td><span class="tick n">—</span></td>'}
      return '<td><span class="tick '+(r.checks[k]?"y":"n")+'">'+(r.checks[k]?"✓":"—")+"</span></td>"}).join("")+"</tr>";
  }).join("")+"</tbody></table>";
  repos.forEach(r=>known.repos.add(r.full));
}
function renderKnow(msgs){
  const cards=[],seen=new Set();
  for(let i=msgs.length-1;i>=0&&cards.length<2;i--){
    const m=msgs[i];if(m.from!=="karmax")continue;
    const x=m.text.match(/^📚 Indexed ([\w./-]+):\n([\s\S]*)$/);
    if(x&&!seen.has(x[1])){seen.add(x[1]);cards.push('<div class=kcard><h5>📚 '+esc(x[1])+'</h5><div class=body>'+md(x[2])+"</div></div>")}
  }
  let sum=[...msgs].reverse().find(m=>m.from==="karmax"&&/^📚 Indexed \d+ repositories/.test(m.text));
  // the latest question that was answered by KARMAX (not an office command)
  let qa="";
  for(let i=msgs.length-1;i>0;i--){
    const m=msgs[i];
    if(m.from==="karmax"&&!/^[🚨🟡👍🛠️🔧✅⛔💬🤖🔴⚠️📦📚🛑ℹ️🔒❌]/u.test(m.text)){
      const q=[...msgs.slice(0,i)].reverse().find(o=>o.from!=="karmax"&&/\?\s*$/.test(o.text));
      if(q){qa='<div class=kcard><h5>Answer from the org\'s knowledge</h5><div class=kq>'+esc(q.from)+": "+esc(q.text)+'</div><div class=body>'+md(m.text)+"</div></div>"}
      break;
    }
  }
  $("knowbody").innerHTML=(qa+(sum?'<div class=kq>'+esc(sum.text)+" · "+hhmm(sum.ts)+"</div>":"")+cards.join(""))||'<div class=empty>Nothing indexed yet — say <code>index all</code>.</div>';
}
let chipState={};
function renderPolicy(p){
  const v=p.values||{},keys=Object.keys(v);
  if(!keys.length&&p.error){$("chips").innerHTML='<span class=chip>'+esc(p.error)+"</span>";return}
  $("chips").innerHTML=keys.map(k=>'<span class="chip '+esc(v[k])+(chipState[k]!==undefined&&chipState[k]!==v[k]?" flash":"")+'">'+esc(k)+": <b>"+esc(v[k])+"</b></span>").join("");
  chipState=Object.assign({},v);
}

/* ---- PR drawer ---- */
async function openPR(repo,n){
  $("dtitle").innerHTML="PR #"+n+"<small>"+esc(repo)+"</small>";$("dbody").innerHTML='<div class=empty>Loading…</div>';
  $("drawer").classList.add("open");$("drawer").setAttribute("aria-hidden","false");$("scrim").hidden=false;
  let j;try{j=await (await fetch("/api/pr?repo="+encodeURIComponent(repo)+"&n="+n)).json()}catch(e){$("dbody").innerHTML='<div class=empty>unreachable</div>';return}
  const rv=[...((state.github.state||{}).reviews||[])].reverse().find(r=>r.repo===repo&&r.number==n);
  const comments=(rv&&rv.comments)||[];
  const pr=j.pr||{};
  $("dtitle").innerHTML="PR #"+n+" · "+esc(pr.title||"")+"<small>"+esc(repo)+" · "+esc((pr.user||{}).login||"")+"</small>";
  let html="";
  if(rv)html+='<div class="rsum '+(rv.event==="REQUEST_CHANGES"?"bad":rv.event==="APPROVE"?"ok":"")+'"><b>'+(rv.event==="REQUEST_CHANGES"?"⛔ Changes requested":rv.event==="APPROVE"?"✅ Approved":"💬 Review")+"</b>\n"+md(rv.body||"")+"</div>";
  else html+='<div class=rsum>No review yet.</div>';
  const used=new Set();
  for(const f of j.files||[]){
    html+='<div class=file><div class=fname>'+esc(f.filename)+' <span style="color:var(--green)">+'+f.additions+'</span> <span style="color:var(--red)">−'+(f.deletions||0)+"</span></div>";
    let o=0,nn=0;
    for(const line of (f.patch||"").split("\n")){
      const h=line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)/);
      if(h){o=+h[1];nn=+h[2];html+='<div class="dl hunk"><span class=ln></span><span class=tx>'+esc(line)+"</span></div>";continue}
      const c=line[0];let cls="",ln="",cm=[];
      if(c==="+"){cls="add";ln=nn;nn++}else if(c==="-"){cls="del";ln=o;o++}else{ln=nn;o++;nn++}
      if(c!=="-")cm=comments.map((x,i)=>[x,i]).filter(([x,i])=>x.line==ln&&(x.path===f.filename||String(x.path||"").split("/").pop()===f.filename.split("/").pop())&&!used.has(i));
      html+='<div class="dl '+cls+(cm.length?" hit":"")+'"><span class=ln>'+ln+'</span><span class=tx>'+esc(line)+"</span></div>";
      for(const [x,i] of cm){used.add(i);const sev=x.severity||(/blocker/i.test(x.body||"")?"blocker":"");
        html+='<div class="cm'+(sev==="warning"?" warn":"")+'"><div class=by>KARMAX'+(sev?' · <span>'+esc(sev)+'</span>':"")+" · "+esc(f.filename)+":"+esc(x.line)+"</div>"+md(x.body||"")+"</div>"}
    }
    html+="</div>";
  }
  const orphan=comments.filter((_,i)=>!used.has(i));
  if(orphan.length)html+="<h4>Other comments</h4>"+orphan.map(x=>'<div class=cm><div class=by>KARMAX · '+esc(x.path||"")+(x.line?":"+esc(x.line):"")+"</div>"+md(x.body||"")+"</div>").join("");
  $("dbody").innerHTML=html;
  const hit=document.querySelector("#dbody .dl.hit");if(hit)setTimeout(()=>hit.scrollIntoView({block:"center",behavior:"smooth"}),150);
}
function closeDrawer(){$("drawer").classList.remove("open");$("drawer").setAttribute("aria-hidden","true");$("scrim").hidden=true}
$("dclose").onclick=$("scrim").onclick=closeDrawer;

/* ---- presenter controls ---- */
function toast(j,ok){
  const t=document.createElement("div");t.className="toast"+(ok?"":" bad");t.textContent=JSON.stringify(j);t.title=t.textContent;
  $("toasts").appendChild(t);setTimeout(()=>t.remove(),6000);
}
async function act(action,args){
  try{
    const r=await fetch("/api/act",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({action,args:args||[]})});
    const j=await r.json();toast(j,r.ok&&j.ok!==false);poll();
  }catch(e){toast({ok:false,error:String(e)},false)}
}
function latestOpenPR(){const r=prRows.find(r=>!r.merges&&r.ci!=="success");return r?r.p.number:(prRows[0]||{p:{}}).p.number}
const BUTTONS=[
  ["Leaked PR",()=>act("leak-pr")],["Clean PR",()=>act("clean-pr")],
  ["CI green",()=>{const n=latestOpenPR();if(n)act("ci",[n,"success"]);else toast({ok:false,error:"no PR"},false)},"ciBtn"],["CI red",()=>{const n=latestOpenPR();if(n)act("ci",[n,"failure"])}],
  ["Break payments",()=>act("break")],["Heal",()=>act("heal")],
  ["Approve",()=>act("approve"),"apprBtn"],["Index all",()=>act("say",["index all"])],
  ["Create repo ledger-svc",()=>act("say",["create repo ledger-svc from go: double-entry ledger"]),null,"wide"],
  ["Ask: What does payments-api depend on?",()=>act("say",["What does payments-api depend on?"]),null,"wide"],
  ["Seed org",()=>act("seed")],["Reset demo",()=>{if(confirm("Reset the demo (down.sh && up.sh)?"))act("reset")},null,"danger"]];
BUTTONS.forEach(([t,fn,id,cls])=>{const b=document.createElement("button");b.textContent=t;if(id)b.id=id;if(cls)b.className=cls;b.onclick=fn;$("ctlgrid").appendChild(b)});
function toggleCtl(){$("ctl").hidden=!$("ctl").hidden}
$("ctlbtn").onclick=toggleCtl;
document.addEventListener("keydown",e=>{
  if(e.key==="Escape")closeDrawer();
  if(e.key==="."&&!/INPUT|TEXTAREA/.test(document.activeElement.tagName))toggleCtl();
});

/* ---- main loop ---- */
let state={github:{}},prRows=[],polling=false;
async function poll(){
  if(polling)return;polling=true;
  try{
    const s=await (await fetch("/api/state")).json();state=s;
    $("banner").hidden=true;
    for(const m of s.messages)if(!seenMsg.has(m.id)){seenMsg.add(m.id);addMsg(m)}
    const inc=incidentFrom(s.messages);
    prRows=prInfo(s.github,s.messages);
    renderPolicy(s.policy);renderProd(s.health,inc);renderInc(inc);renderPRs(prRows);renderRepos(s.github.repos||[],s.github);renderKnow(s.messages);
    const b=$("apprBtn");b.disabled=!s.pending;b.textContent=s.pending?"Approve "+s.pending:"Approve";
    const n=latestOpenPR();$("ciBtn").textContent=n?"CI green for #"+n:"CI green";
    if(!s.github.ok){$("banner").hidden=false;$("banner").textContent="GitHub unreachable — "+(s.github.error||"")}
  }catch(e){$("banner").hidden=false;$("banner").textContent="Console server unreachable — retrying…"}
  polling=false;
}
setInterval(poll,1200);poll();
setInterval(()=>$("clock").textContent=new Date().toLocaleTimeString([],{hour:"2-digit",minute:"2-digit",second:"2-digit"}),500);
