'use strict';
// 听说 TingShuo 桌面前端（开发版）
// 架构说明：生产 BYOK 形态下，LLM/Azure 由浏览器直连（FR-3.12）；
// 开发期经 /api/v1/dev/* 本地代理转发（生产 DEV_PROXY=false 关闭）。

// ============ 基础 ============
const $ = id => document.getElementById(id);
const tokenKey = 'ts_token';
let TOKEN = localStorage.getItem(tokenKey) || '';
let CFG = {};        // /dev/config 结果
let LESSON = null;   // 当前课程全量
let CUR_MAT = 0;     // 当前材料 tab
let playing = null;  // 当前 audio

function go(name) {
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('on'));
  $('sc-' + name).classList.add('on');
}
function showErr(id, msg) { const e = $(id); e.textContent = msg; e.style.display = 'block'; }
function hideErr(id) { $(id).style.display = 'none'; }
function esc(s) { return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

async function api(path, opts = {}) {
  const r = await fetch('/api/v1' + path, {
    ...opts,
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: 'Bearer ' + TOKEN } : {}),
      ...(opts.headers || {}),
    },
  });
  const body = await r.json().catch(() => ({}));
  if (r.status >= 300) throw new Error(body?.error?.message || ('HTTP ' + r.status));
  return body;
}

// ============ 登录 ============
async function doLogin() {
  hideErr('login-err');
  const email = $('login-email').value.trim(), pass = $('login-pass').value;
  if (!email || pass.length < 6) return showErr('login-err', '请输入邮箱与至少 6 位密码');
  for (const path of ['/auth/register', '/auth/login']) {
    try {
      const r = await api(path, { method: 'POST', body: JSON.stringify({ email, password: pass, nickname: '试用' }) });
      TOKEN = r.access_token; localStorage.setItem(tokenKey, TOKEN);
      return afterLogin();
    } catch (e) {
      if (path === '/auth/login') return showErr('login-err', e.message);
    }
  }
}
async function afterLogin() {
  $('userchip').textContent = $('login-email').value.trim();
  CFG = await api('/dev/config').catch(() => ({}));
  await loadHome();
}
async function loadHome() {
  const today = new Date().toISOString().slice(0, 10);
  try {
    const list = await api(`/lessons?from=${today}&to=${today}`);
    const it = (list.lessons || [])[0];
    if (it && it.status === 'ready') {
      LESSON = await api('/lessons/' + it.lesson_id);
      go('learn'); renderLearn();
    } else if (it) {
      window._draftLesson = it.lesson_id;
      go('create');
    } else {
      go('create');
    }
  } catch (e) { showErr('home-err', e.message); go('create'); }
}

// ============ 词单 ============
$('words-input').addEventListener('input', countWords);
function countWords() {
  const n = parseWords($('words-input').value).length;
  let tip = '';
  if (n === 0) tip = '（还没输入）';
  else if (n === 1) tip = '（一个词也可以生成）';
  else if (n < 5) tip = '（词少也可以生成）';
  else if (n > 40) tip = '（词较多，生成与学习时间会变长）';
  $('words-count').textContent = n + ' 词' + tip;
}
function parseWords(text) {
  const out = [];
  for (const line of text.split('\n')) {
    const s = line.trim();
    if (!s) continue;
    const isKey = s.includes('★');
    const parts = s.replace('★', '').split(/[｜|\t]/).map(x => x.trim());
    if (!parts[0]) continue;
    out.push({
      surface: parts[0], reading: parts[1] || '', pos: parts[2] || '',
      meaning: parts[3] || '', is_key: isKey,
    });
  }
  return out;
}
function fillSample() {
  $('words-input').value =
`精算｜せいさん｜名詞｜一起结账 ★
預ける｜あずける｜動詞｜寄存；存放 ★
満員｜まんいん｜名詞｜满座
駅｜えき｜名詞｜车站
切符｜きっぷ｜名詞｜票
掃除｜そうじ｜名詞｜清扫
洗濯｜せんたく｜名詞｜洗衣服
予約｜よやく｜名詞｜预约
用意｜ようい｜名詞｜准备
届く｜とどく｜動詞｜送达
選ぶ｜えらぶ｜動詞｜选择
疲れる｜つかれる｜動詞｜疲惫
寄る｜よる｜動詞｜顺路去
揃う｜そろう｜動詞｜齐全
貯金｜ちょきん｜名詞｜存钱
趣味｜しゅみ｜名詞｜爱好
頑張る｜がんばる｜動詞｜坚持、加油
家賃｜やちん｜名詞｜房租
説明｜せつめい｜名詞｜说明
替わる｜かわる｜動詞｜更换`;
  countWords();
}

// ============ 提交 + 生成 ============
function genLog(msg) {
  const el = $('gen-log'); el.style.display = 'block';
  el.textContent = '[' + new Date().toLocaleTimeString() + '] ' + msg + '\n' + el.textContent;
}
async function submitWords() {
  hideErr('create-err'); hideErr('gen-err');
  const words = parseWords($('words-input').value);
  if (words.length === 0) return showErr('create-err', '请至少输入 1 个词');
  if (words.length > 200) return showErr('create-err', '词数过多（上限 200，防误粘贴超大文本）');
  $('gen-retry').style.display = 'none'; $('gen-log').style.display = 'none'; $('gen-log').textContent = '';
  $('gen-title').textContent = '材料生成中';
  $('gen-note').innerHTML = '正在用大模型围绕你的词单写对话和短文，通常需要 2—5 分钟。<br>请不要关闭本页。';
  go('gen');
  try {
    // 1. 建课（同日复用 draft）
    const today = new Date().toISOString().slice(0, 10);
    let lessonID;
    try {
      const r = await api('/lessons', {
        method: 'POST',
        body: JSON.stringify({
          lesson_date: today, words,
          params: { scenes: ['便利店', '车站', '家'], level: $('sel-level').value },
        }),
      });
      lessonID = r.lesson_id;
    } catch (e) {
      if (String(e.message).includes('已存在')) {
        const list = await api(`/lessons?from=${today}&to=${today}`);
        lessonID = list.lessons[0].lesson_id;
      } else throw e;
    }
    genLog('课程就绪 ' + lessonID);

    // 2. LLM 生成材料（后台任务 + 轮询：刷新页面/网络抖动都不丢进度）
    const prompt = buildPrompt(words);
    genLog('调用大模型生成材料…');
    let llmText = '', fullLen = 0, llmErr = '';
    const t0 = Date.now();
    $('gen-preview').style.display = 'block';
    const started = await api('/dev/llm/start', {
      method: 'POST',
      body: JSON.stringify({ model: CFG.llm_model, messages: [{ role: 'user', content: prompt }] }),
    });
    const taskID = started.task_id;
    genLog('生成任务已启动（服务端后台执行，刷新页面也不会中断）');
    // 轮询循环
    while (true) {
      await new Promise(r => setTimeout(r, 1000));
      let st;
      try {
        st = await api('/dev/llm/poll?id=' + taskID);
      } catch (e) {
        continue; // 网络抖动容忍：继续轮询，服务端任务不受影响
      }
      llmText = st.text || ''; fullLen = st.chars || 0;
      const sec = Math.floor(st.elapsed_ms / 1000);
      if (fullLen > 0) {
        $('gen-preview').textContent = '…' + llmText.slice(-150).replace(/\s+/g, ' ');
      }
      $('gen-step').textContent =
        `已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 模型已输出 ${fullLen} 字符`;
      if (st.err) { llmErr = st.err; break; }
      if (st.done) break;
    }
    if (llmErr) throw new Error('LLM 生成失败：' + llmErr);
    if (!llmText.trim()) throw new Error('LLM 返回为空，请重试');
    const genSec = Math.floor((Date.now() - t0) / 1000);
    genLog(`生成完毕（${fullLen} 字符，耗时 ${genSec} 秒）`);
    $('gen-step').textContent = `生成完成（${fullLen} 字符 / ${genSec} 秒），正在校验落库…`;
    $('gen-preview').textContent = '';
    const out = JSON.parse(largestJSON(llmText));
    genLog('材料解析成功（' + (out.materials || []).length + ' 段）');

    // 3. 校验 + surface → word_id 重映射
    const full = await api('/lessons/' + lessonID);
    const idBySurface = {};
    for (const w of full.words) idBySurface[w.surface] = w.id;
    const occ = {};
    for (const [k, v] of Object.entries(out.word_occurrence || {})) occ[idBySurface[k] || k] = v;
    out.word_occurrence = occ;
    const problems = validateLocally(out, full.words);
    if (problems.length) throw new Error('材料校验未通过：' + problems.join('；'));
    genLog('校验通过，上传落库…');

    // 4. 上传
    const up = await api(`/lessons/${lessonID}/materials`, { method: 'POST', body: JSON.stringify(out) });
    genLog('落库成功 status=' + up.status + (up.warnings?.length ? '（注音警告 ' + up.warnings.length + ' 句）' : ''));

    LESSON = await api('/lessons/' + lessonID);
    go('learn'); renderLearn();
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-retry').style.display = 'block';
    $('gen-title').textContent = '生成失败';
  }
}

function validateLocally(out, words) {
  const problems = [];
  const sids = new Set();
  for (const m of out.materials || []) {
    for (const s of m.sentences || []) sids.add(s.id);
    const n = (m.sentences || []).length;
    if (n < 8 || n > 12) problems.push(`材料 ${m.id} 句数 ${n} 超出 8—12`);
  }
  for (const w of words) {
    const occ = (out.word_occurrence || {})[w.id];
    if (!occ) { problems.push(`词 ${w.surface} 未在复现表交代`); continue; }
    const list = Array.isArray(occ) ? occ : (occ.sentences || []);
    const skipped = typeof occ === 'string' && occ.startsWith('skipped');
    if (!skipped && list.length === 0) problems.push(`词 ${w.surface} 复现列表为空`);
    if (!skipped && w.is_key && list.length < 2) problems.push(`重点词 ${w.surface} 仅出现 1 次（需 2 次）`);
  }
  return problems;
}

function buildPrompt(words) {
  const lines = words.map(w => `${w.surface}｜${w.reading || '—'}｜${w.meaning}${w.is_key ? '｜★重点：出现 2 次' : ''}`);
  return `你是日语听说学习材料的编写专家。请为下面的词汇表生成一份完整的 JSON 材料包。

要求：
- 难度贴合指定水平，全部句子用です・ます体，自然、贴近日常生活
- 每个词都必须自然地出现在至少一个句子里；确实不适合场景时才允许标 "skipped: 原因"
- 生成恰好 2 段材料：
  1. id=m1，type=dialogue，场景「コンビニ」，8—10 句，speaker 用 A/B 交替，role_a=店員，role_b=客
  2. id=m2，type=passage，主题「休日の家事」，8—10 句独白，speaker=独白
- 句子 id 用 s1、s2、s3…（跨材料顺序编号）；sort_order 从 1 开始
- 每句包含 text（日语）、kana（整句假名读音）、translation（中文翻译）
- qa_items 恰好 5 个（prompt、answer、prompt_translation）
- substitution_items 恰好 5 个（word_id 用词的 surface、prompt_zh、answer）
- word_occurrence：key 是词的 surface，value 是句子 id 数组（如 ["s1","s3"]）或 "skipped: 原因"

词汇表（surface｜读音｜中文｜标记）：
${lines.join('\n')}

严格按如下 JSON 结构输出（不要 markdown 围栏、不要解释）：
{"materials":[{"id":"m1","type":"dialogue","scene":"...","title":"...","role_a":"店員","role_b":"客","sort_order":1,"sentences":[{"id":"s1","sort_order":1,"speaker":"A","text":"...","kana":"...","translation":"...","para_no":0}]}],"qa_items":[{"prompt":"...","answer":"...","prompt_translation":"..."}],"substitution_items":[{"word_id":"精算","prompt_zh":"...","answer":"..."}],"word_occurrence":{"精算":["s1","s3"]}}`;
}

// 最大平衡 JSON 块提取（推理模型可能混入思考文本，见开发问题记录 P-023）
function largestJSON(s) {
  let best = '';
  for (let i = 0; i < s.length; i++) {
    if (s[i] !== '{') continue;
    let depth = 0, inStr = false, esc = false;
    for (let j = i; j < s.length; j++) {
      const c = s[j];
      if (esc) { esc = false; continue; }
      if (inStr) { if (c === '\\') esc = true; else if (c === '"') inStr = false; continue; }
      if (c === '"') inStr = true;
      else if (c === '{') depth++;
      else if (c === '}') {
        depth--;
        if (depth === 0) {
          const cand = s.slice(i, j + 1);
          if (cand.length > best.length) { try { JSON.parse(cand); best = cand; } catch {} }
          break;
        }
      }
    }
  }
  if (best) return best;
  const a = s.indexOf('{'), b = s.lastIndexOf('}');
  return s.slice(a, b + 1);
}

// ============ 学习屏 ============
function rubyHTML(tokens) {
  if (!Array.isArray(tokens) || !tokens.length) return '';
  return tokens
    .map(([w, r]) => (r && /[一-龯]/.test(w)) ? `<ruby>${esc(w)}<rt>${esc(r)}</rt></ruby>` : esc(w))
    .join('');
}

function renderLearn() {
  const mats = LESSON.materials || [];
  const tabs = $('mat-tabs'); tabs.innerHTML = '';
  mats.forEach((m, i) => {
    const b = document.createElement('button');
    b.textContent = (m.title || m.scene || ('材料 ' + (i + 1)));
    b.className = i === CUR_MAT ? 'on' : '';
    b.onclick = () => { CUR_MAT = i; renderLearn(); };
    tabs.appendChild(b);
  });
  const m = mats[CUR_MAT] || { sentences: [] };
  const box = $('sents'); box.innerHTML = '';
  m.sentences.forEach((s, idx) => {
    const has = (s.audios || []).length > 0;
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(s.speaker || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
      <div class="zh">${esc(s.translation || '')}</div></div>
      <button class="play ${has ? 'has' : ''}" title="${has ? '播放' : '未配音'}">${has ? '▶' : '…'}</button>`;
    div.onclick = () => playSentence(s, idx, div);
    box.appendChild(div);
  });
  const total = mats.reduce((n, x) => n + x.sentences.length, 0);
  const withAudio = mats.reduce((n, x) => n + x.sentences.filter(s => (s.audios || []).length).length, 0);
  $('learn-hint').textContent = `共 ${total} 句 / 已配音 ${withAudio} 句 · 点句子播放 · 顶部切换模式`;
  $('tts-btn').style.display = 'none';
  if (withAudio < total) {
    $('tts-btn').style.display = 'block';
    $('tts-btn').textContent = '为全部句子配音';
  }
}

function setMode(mode) {
  const blind = mode === 'blind', zh = mode === 'zh';
  document.querySelectorAll('#sents .txt').forEach(el => (el.style.filter = blind ? 'blur(6px)' : 'none'));
  document.querySelectorAll('#sents .sent').forEach(el => el.classList.toggle('showzh', zh));
  $('md-normal').classList.toggle('on', !blind && !zh);
  $('md-blind').classList.toggle('on', blind);
  $('md-zh').classList.toggle('on', zh);
}
function toggleRT() {
  const off = document.body.classList.toggle('no-rt');
  $('md-rt').classList.toggle('on', !off);
}

async function audioURL(s) {
  const a = (s.audios || [])[0];
  if (!a) return null;
  const r = await fetch('/api/v1/audio/' + a.id + '/file',
    { redirect: 'manual', headers: { Authorization: 'Bearer ' + TOKEN } });
  if (r.status !== 302) throw new Error('音频 302 失败 HTTP ' + r.status);
  return r.headers.get('Location');
}

async function playSentence(s, idx, div) {
  try {
    stopPlay();
    document.querySelectorAll('.sent').forEach(x => x.classList.remove('now'));
    div.classList.add('now');
    let url = s._url || await audioURL(s);
    if (!url) return;
    s._url = url; // 预签名 URL（10 分钟内有效，页面级缓存复用）
    playing = new Audio(url);
    playing.onended = () => { div.classList.remove('now'); playing = null; };
    playing.play();
  } catch (e) { alert(e.message); }
}
function stopPlay() { if (playing) { playing.pause(); playing = null; } }

async function playAll() {
  const sents = (LESSON.materials[CUR_MAT] || {}).sentences || [];
  for (let i = 0; i < sents.length; i++) {
    const s = sents[i];
    if (!(s.audios || []).length) continue;
    const div = document.querySelector(`.sent[data-idx="${i}"]`);
    await new Promise(res => {
      playSentence(s, i, div);
      const wait = setInterval(() => { if (!playing) { clearInterval(wait); res(); } }, 300);
    });
  }
}

// ============ 批量配音 ============
async function genAllAudio() {
  const btn = $('tts-btn'); btn.disabled = true;
  const voice = $('sel-voice').value;
  const all = (LESSON.materials || []).flatMap(m => m.sentences);
  let done = 0, ok = 0, fail = 0;
  btn.textContent = `配音中 0/${all.length}…`;
  for (const s of all) {
    if ((s.audios || []).length) { done++; ok++; continue; }
    try {
      const ck = await api('/audio/check', {
        method: 'POST',
        body: JSON.stringify({
          items: [{ sentence_id: s.id, text: s.text, voice, rate: 1.0, style: '', engine_ver: 'azure-v1' }],
        }),
      });
      const item = ck.items[0];
      if (item.exists) { s.audios = [{ id: item.audio_id }]; done++; ok++; continue; }
      // 生成（开发经 /dev/tts；生产为浏览器 BYOK 直连 Azure）
      const mp3 = await fetch('/api/v1/dev/tts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + TOKEN },
        body: JSON.stringify({ text: s.text, voice, rate: 1.0 }),
      });
      if (!mp3.ok) throw new Error('TTS HTTP ' + mp3.status);
      const blob = await mp3.blob();
      const pr = await api('/audio/presign', { method: 'POST', body: JSON.stringify({ cache_key: item.cache_key }) });
      const up = await fetch(pr.upload_url, { method: 'PUT', body: blob, headers: { 'Content-Type': 'audio/mpeg' } });
      if (!up.ok) throw new Error('上传 HTTP ' + up.status);
      const reg = await api('/audio/register', {
        method: 'POST',
        body: JSON.stringify({
          sentence_id: s.id, cache_key: item.cache_key, voice, rate: 1.0, style: '',
          engine_ver: 'azure-v1', duration_ms: 0, size_bytes: blob.size,
        }),
      });
      s.audios = [{ id: reg.audio_id }];
      ok++;
    } catch (e) { fail++; console.error(e); }
    done++;
    btn.textContent = `配音中 ${done}/${all.length}…`;
  }
  btn.disabled = false;
  LESSON = await api('/lessons/' + LESSON.lesson.id);
  renderLearn();
  alert(`配音完成：成功 ${ok} / 失败 ${fail}（失败句可重新点击「为全部句子配音」补齐）`);
}

// ============ 启动 ============
if (TOKEN) afterLogin(); else go('login');
