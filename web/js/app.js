'use strict';
// 听说 TingShuo 桌面前端（开发版）
// 架构说明：生产 BYOK 形态下，LLM/Azure 由浏览器直连（FR-3.12）；
// 开发期经 /api/v1/dev/* 本地代理转发（生产 DEV_PROXY=false 关闭）。

// ============ 基础 ============
const $ = id => document.getElementById(id);
const tokenKey = 'ts_token', refreshKey = 'ts_refresh', emailKey = 'ts_email';
let TOKEN = localStorage.getItem(tokenKey) || '';
let REFRESH = localStorage.getItem(refreshKey) || '';
let EMAIL = localStorage.getItem(emailKey) || ''; // 刷新后登录框为空，账号名从这里恢复
let CFG = {};        // /config 结果（仅 llm_model；Key 永不下放客户端）
let LESSON = null;   // 当前课程全量
let CUR_MAT = 0;     // 当前材料 tab
let CUR_MODE = 'normal'; // 跟读/盲听/译文（重渲染后需恢复）
let VIEW = 'material';   // material=当前材料 / fav=收藏视图
let playing = null;  // 当前 audio
let FAVS = new Set();    // 已收藏句子 ID
let FAV_ITEMS = [];      // 收藏句列表（含内容）
let FAVM = new Set();    // 已收藏材料 ID
let RV_POOL = new Set(); // 已在复习池的句子 ID（含已掌握）

// ============ 页面导航与「返回上一级」============
// 返回按访问顺序：每次屏间跳转 pushState（同 URL，state 记屏名/深度/本页会话 sid），
// 浏览器后退与左上角「返回」都走 history.back；popstate 时恢复对应屏。
// 页面树仅作兜底：历史里没有可退的屏（如刷新后直接落在某屏）时按层级返回。
// home 为根；create/learn/review/library/wordstudy/notebook 为一级；
// gen 是中转页（生成中），其上一级 = 进入它时的来源页（create 或 learn）。
const NAV_PARENT = {
  home: null, login: null,
  create: 'home', learn: 'home', review: 'home',
  library: 'home', wordstudy: 'home', notebook: 'home',
  recite: 'home', 'recite-play': 'recite',
  aiask: 'notebook',
};
let CUR_SCREEN = 'login';
let GEN_FROM = 'create';

const HIST_SID = Date.now().toString(36) + Math.random().toString(36).slice(2);
let HIST_BASE = false; // 本页会话的首条历史已登记
let HIST_DEPTH = 0;    // 当前屏在会话历史里的深度（0 = 起点）

function histState(name) {
  return { tsSid: HIST_SID, tsScreen: name, tsDepth: HIST_DEPTH, tsGenFrom: GEN_FROM };
}
function histOwn() {
  const s = history.state;
  return !!(s && s.tsSid === HIST_SID);
}
function applyScreen(name) {
  if (CUR_SCREEN !== name) clipStopAll();
  if (CUR_SCREEN === 'recite-play' && name !== 'recite-play') recLeave();
  CUR_SCREEN = name;
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('on'));
  $('sc-' + name).classList.add('on');
  updateBackBtn();
}

// go 切屏并登记历史：登录转换、离开 gen 中转页、同屏重入都替换当前条目
// （不占历史）；其余屏间跳转压入新条目，浏览器后退键据此回上一屏。
function go(name, opts = {}) {
  if (name === 'gen' && CUR_SCREEN !== 'gen') GEN_FROM = CUR_SCREEN; // 记录中转页来源（重入 gen 不覆盖）
  const prev = CUR_SCREEN;
  applyScreen(name);
  if (!HIST_BASE || !histOwn()) { // 首个屏或历史已漂到别处：重新登记会话起点
    HIST_BASE = true;
    HIST_DEPTH = 0;
    history.replaceState(histState(name), '', location.href);
    return;
  }
  if (name === prev && !opts.replace) return;
  if (opts.replace || name === 'login' || prev === 'login' || prev === 'gen') {
    history.replaceState(histState(name), '', location.href);
    return;
  }
  HIST_DEPTH += 1;
  history.pushState(histState(name), '', location.href);
}

// goBack 左上角返回：优先按访问顺序后退；没有站内历史时按页面树兜底
function goBack() {
  if (CUR_SCREEN === 'home' || CUR_SCREEN === 'login') return;
  if (HIST_BASE && histOwn() && (history.state.tsDepth || 0) > 0) {
    history.back();
    return;
  }
  const target = CUR_SCREEN === 'gen' ? GEN_FROM : (NAV_PARENT[CUR_SCREEN] || 'home');
  go(target, { replace: true });
}

// popstate：弹层先收；其余按历史条目恢复屏（旧页面会话的条目不恢复——
// 课程/列表数据已随上次加载丢失，浏览器继续后退即离开本站）
addEventListener('popstate', e => {
  const masks = ['picker-mask', 'nb-cfg-mask'];
  if (masks.some(id => $(id) && getComputedStyle($(id)).display !== 'none')) {
    masks.forEach(id => { $(id).style.display = 'none'; });
    if (HIST_BASE) history.pushState(histState(CUR_SCREEN), '', location.href); // 把退掉的这步压回，保持当前屏
    return;
  }
  const st = e.state;
  if (!HIST_BASE || !st || st.tsSid !== HIST_SID) return;
  const name = st.tsScreen;
  if (!name || !$('sc-' + name)) return;
  HIST_DEPTH = st.tsDepth || 0;
  if (st.tsGenFrom) GEN_FROM = st.tsGenFrom;
  applyScreen(name);
});

function updateBackBtn() {
  const b = $('nav-back');
  if (b) b.style.display = (CUR_SCREEN === 'home' || CUR_SCREEN === 'login') ? 'none' : 'inline-flex';
}
function showErr(id, msg) { const e = $(id); e.textContent = msg; e.style.display = 'block'; }
function hideErr(id) { $(id).style.display = 'none'; }
function esc(s) { return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

// toast 底部提示：播放/请求失败可见，不再静默
function toast(msg) {
  let t = $('ts-toast');
  if (!t) { t = document.createElement('div'); t.id = 'ts-toast'; document.body.appendChild(t); }
  t.textContent = msg;
  t.classList.add('on');
  clearTimeout(t._h);
  t._h = setTimeout(() => t.classList.remove('on'), 4000);
}

// fetchAuth：401 自动刷新 token 重试一次（音频等裸 fetch 与 api() 同待遇）
async function fetchAuth(url, opts = {}) {
  opts.headers = Object.assign({ Authorization: 'Bearer ' + TOKEN }, opts.headers);
  let r = await fetch(url, opts);
  if (r.status === 401 && (await doRefresh())) {
    opts.headers.Authorization = 'Bearer ' + TOKEN;
    r = await fetch(url, opts);
  }
  return r;
}

// playUnlock：移动端浏览器可能拒绝手势外的 play()——先静音解锁播放再恢复音量
async function playUnlock(a) {
  // 安卓 Chrome 上 'playing' 可能在监听挂载前就触发，导致静音卡死：挂载提前 + 播成后立即解除
  const unmute = () => { a.muted = false; };
  a.addEventListener('playing', unmute, { once: true });
  a.addEventListener('timeupdate', unmute, { once: true });
  try {
    await a.play();
  } catch (e) {
    if (e && e.name === 'NotAllowedError') {
      a.muted = true;
      await a.play();
      a.muted = false; // 播起来就够了，立刻解除，不等事件（事件可能早已错过）
      setTimeout(unmute, 250);
    } else {
      throw e;
    }
  }
}

// wsAudioDbg 播放状态行：只在异常时显示（静音/报错/不走字），正常播放不打扰界面
function wsAudioDbg(a, tag) {
  const el = document.getElementById('ws-dbg');
  if (!el) return;
  const bad = a && (a.muted || a.error || (!a.paused && a.currentTime < 0.05) || (a.paused && a.currentTime > 0 && !a.ended));
  el.textContent = bad
    ? `${tag} 播放 ${a.currentTime.toFixed(1)}s · 静音 ${a.muted ? '是' : '否'} · 暂停 ${a.paused ? '是' : '否'} · 错误 ${a.error ? a.error.code : '无'}`
    : '';
}

// doRefresh 用 refresh_token 换新 access_token（并发 401 共享同一次刷新）
let refreshing = null;
async function doRefresh() {
  if (!REFRESH) return false;
  refreshing = refreshing || (async () => {
    try {
      const r = await fetch('/api/v1/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: REFRESH }),
      });
      if (!r.ok) return false;
      const b = await r.json();
      TOKEN = b.access_token; localStorage.setItem(tokenKey, TOKEN);
      return true;
    } catch { return false; }
    finally { setTimeout(() => (refreshing = null), 0); }
  })();
  return refreshing;
}

function clearSession() {
  localStorage.removeItem(tokenKey); localStorage.removeItem(refreshKey);
  localStorage.removeItem(emailKey);
  TOKEN = ''; REFRESH = ''; EMAIL = '';
}

// doLogout 登出：清会话并整页重载，落回登录屏
function doLogout() {
  stopPlay();
  if (RV_AUDIO) { RV_AUDIO.pause(); RV_AUDIO = null; }
  clearSession();
  location.reload();
}

async function api(path, opts = {}) {
  const call = () => fetch('/api/v1' + path, {
    ...opts,
    headers: {
      'Content-Type': 'application/json',
      ...(TOKEN ? { Authorization: 'Bearer ' + TOKEN } : {}),
      ...(opts.headers || {}),
    },
  });
  let r = await call();
  // access 过期 → 自动续期并重试一次（无感续期）
  if (r.status === 401 && REFRESH && TOKEN) {
    if (await doRefresh()) r = await call();
  }
  if (r.status === 401) {
    // refresh 也失效：清会话并整页重载——所有数据已失效，
    // 重载后 app.js 以空会话初始化，自然落在登录屏
    clearSession();
    location.reload();
    throw new Error('登录已过期，请重新登录');
  }
  const body = await r.json().catch(() => ({}));
  if (r.status >= 300) throw new Error(body?.error?.message || ('HTTP ' + r.status));
  return body;
}

// ============ LLM 生成小助手（服务端持 Key 代生成，任务落 generation_job）============
async function startGen(lessonID, prompt) {
  const r = await api('/generate/start', {
    method: 'POST',
    body: JSON.stringify({ lesson_id: lessonID, messages: [{ role: 'user', content: prompt }] }),
  });
  return r.task_id;
}
function pollGen(id) { return api('/generate/poll?id=' + id); }

// ============ 登录 ============
async function doLogin() {
  hideErr('login-err');
  const email = $('login-email').value.trim(), pass = $('login-pass').value;
  if (!email || pass.length < 6) return showErr('login-err', '请输入邮箱与至少 6 位密码');
  for (const path of ['/auth/register', '/auth/login']) {
    try {
      // 登录 body 不能带 nickname（注册专属字段，服务端严格解析会 400）
      const payload = path === '/auth/login'
        ? { email, password: pass }
        : { email, password: pass, nickname: '试用' };
      const r = await api(path, { method: 'POST', body: JSON.stringify(payload) });
      TOKEN = r.access_token; localStorage.setItem(tokenKey, TOKEN);
      REFRESH = r.refresh_token || ''; localStorage.setItem(refreshKey, REFRESH);
      EMAIL = email; localStorage.setItem(emailKey, EMAIL);
      return afterLogin();
    } catch (e) {
      if (path === '/auth/login') return showErr('login-err', e.message);
    }
  }
}
async function afterLogin() {
  if (!EMAIL) { // 旧会话无缓存邮箱（修复前登录的）：从 /me 补
    try {
      const me = await api('/me');
      EMAIL = me.email || ''; localStorage.setItem(emailKey, EMAIL);
    } catch {}
  }
  $('userchip').textContent = EMAIL;
  $('logout-btn').style.display = 'inline-flex';
  hideErr('home-err');
  CFG = await api('/config').catch(() => ({}));
  await loadFavs();
  await loadReviewPool();
  await loadHome();
}

// loadFavs 拉取收藏句/材料（学习屏星标与收藏视图的数据源）
async function loadFavs() {
  try {
    const fs = await api('/favorites/sentences');
    FAV_ITEMS = fs.items || [];
    FAVS = new Set(FAV_ITEMS.map(x => x.sentence_id));
  } catch {}
  try {
    const fm = await api('/favorites/materials');
    FAVM = new Set(fm.material_ids || []);
  } catch {}
}

// loadReviewPool 拉取复习池句子 ID（学习屏"加入复习"按钮状态用）
async function loadReviewPool() {
  try {
    const r = await api('/reviews/pool');
    RV_POOL = new Set([...(r.active || []), ...(r.mastered || [])]);
  } catch {}
}

// goHome 回首页并刷新今日课程状态（学习屏的返回入口）
async function goHome() {
  stopPlay();
  if (RV_AUDIO) { RV_AUDIO.pause(); RV_AUDIO = null; }
  await loadFavs();
  await loadReviewPool();
  await loadHome();
}

// switchView 学习屏内切换：当前材料 / 生词短语 / 生词 / 收藏列表
async function switchView(v) {
  if (VIEW === v) return;
  VIEW = v; CUR_MAT = 0;
  if (v === 'fav') await loadFavs();
  $('md-fav').classList.toggle('on', v === 'fav');
  // 内容切换栏高亮（收藏视图时第二栏全灭）
  $('vw-words').classList.toggle('on', v === 'words');
  $('vw-phrases').classList.toggle('on', v === 'phrases');
  $('vw-full').classList.toggle('on', v === 'material');
  $('md-normal').classList.toggle('on', v === 'material' && CUR_MODE === 'normal');
  if (v === 'fav') renderFavView();
  else if (v === 'phrases') renderPhrasesView();
  else if (v === 'words') renderWordsView();
  else renderLearn();
}

// phrasesMaterial 取当前课程的生词短语材料（type='phrases'）
function phrasesMaterial() {
  return (LESSON.materials || []).find(m => m.type === 'phrases') || null;
}

// renderWordsView 生词视图：本课词列表 → 点词展开用法索引（短语 + 按场景的句子，可点播）
function renderWordsView() {
  $('mat-tabs').style.display = 'none';
  $('tts-btn').style.display = 'none';
  const box = $('sents'); box.innerHTML = '';
  const words = LESSON.words || [];
  if (!words.length) {
    $('learn-hint').textContent = '本课无生词';
    return;
  }
  words.forEach((w, idx) => {
    const div = document.createElement('div');
    div.className = 'sent wordrow'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${w.is_key ? '★' : ''}</span>
      <div class="wrap">
        <div class="txt"><ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby></div>
        <div class="zh">${esc(w.meaning || '')} ${esc(w.pos || '')}</div>
      </div>
      <button class="play" title="展开用法">▾</button>`;
    div.onclick = () => toggleWordUsage(w, div);
    box.appendChild(div);
  });
  applyMode();
  $('learn-hint').textContent = `生词 · 共 ${words.length} 个 · 点词展开它的短语与句子（可点播）`;
}

// toggleWordUsage 词的用法索引：GET /words/{id}/usage → 短语 + 按场景句子
async function toggleWordUsage(w, div) {
  const existing = div.querySelector('.usage');
  if (existing) { clipStopAll(); existing.remove(); return; } // 再次点击收起
  clipStopAll();
  document.querySelectorAll('.usage').forEach(u => u.remove()); // 同时只展开一个
  const box = document.createElement('div');
  box.className = 'usage';
  box.innerHTML = '<p class="hint">加载用法…</p>';
  div.appendChild(box);
  try {
    const u = await api('/words/' + w.id + '/usage');
    const phrases = (u.items || []).filter(x => x.type === 'phrases');
    const sents = (u.items || []).filter(x => x.type !== 'phrases');
    let html = '';
    if (phrases.length) {
      html += '<div class="usage-head">生词短语</div>';
      phrases.forEach(x => (html += usageRow(x)));
    }
    if (sents.length) {
      html += '<div class="usage-head">句子（按场景）</div>';
      sents.forEach(x => {
        html += `<div class="usage-scene">${esc(x.title || x.scene || '—')} · ${esc(x.lesson_date)}</div>`;
        html += usageRow(x);
      });
    }
    if (!html) html = '<p class="hint">该词暂无关联句子（此课的材料生成于关联功能上线前，重新生成可建立索引）。</p>';
    box.innerHTML = html;
    // 绑定点播（阻止冒泡到词行——否则会触发 toggleWordUsage 的收起逻辑）
    box.querySelectorAll('.usage-sent').forEach((row, i) => {
      const sid = row.dataset.sid;
      const it = (u.items || []).find(x2 => x2.sentence_id === sid);
      row.onclick = (e) => {
        e.stopPropagation();
        const s = {
          id: it.sentence_id, text: it.text, ruby_tokens: it.ruby_tokens,
          translation: it.translation,
          audios: it.audio_id ? [{ id: it.audio_id }] : [],
        };
        playOne(s, i, row);
      };
    });
  } catch (e) {
    box.innerHTML = '<p class="hint">加载失败：' + esc(e.message) + '</p>';
  }
  // 原声台词：异步加载，不阻塞用法面板
  const clips = document.createElement('div');
  clips.className = 'wclip-box';
  box.appendChild(clips);
  loadWordClips(w, clips);
}

// usageRow 用法条目行（点播）
function usageRow(x) {
  const has = !!x.audio_id;
  return `
    <div class="sent usage-sent" data-sid="${esc(x.sentence_id)}">
      <span class="spk">${x.type === 'phrases' ? '短' : '句'}</span>
      <div class="wrap"><div class="txt">${rubyHTML(x.ruby_tokens) || esc(x.text)}</div>
      <div class="zh">${esc(x.translation || '')}</div></div>
      <button class="play ${has ? 'has' : ''}">${has ? '▶' : '…'}</button>
    </div>`;
}

// ============ 生词原声例句（剧照 + 原声）============
// Nadeshiko 的「视频」实际是静帧+音频，这里按剧照呈现：点图播原声。
// 后端按词缓存最多 5 条，前端每次展示 2 条，点"换两条"本地轮换（不消耗 API 配额）。
const CLIP_MEM = new Map();

function clipStopAll() {
  document.querySelectorAll('.wclip-audio').forEach(x => { try { x.pause(); } catch {} });
  document.querySelectorAll('.wclip-mini.on, .wclip-poster.on').forEach(b => b.classList.remove('on'));
  document.querySelectorAll('.wclip-play').forEach(b => { b.textContent = '▶'; });
}

function clipPauseOthers(el) {
  if (playing) { playing.pause(); playing = null; }
  if (WS_AUDIO) { WS_AUDIO.pause(); WS_AUDIO = null; }
  if (RV_AUDIO) RV_AUDIO.pause();
  document.querySelectorAll('.wclip-audio').forEach(x => {
    if (x !== el && !x.paused) x.pause();
  });
}

function clipTextHTML(c) {
  const t = (c.text_ja || '').replace(/\u3000/g, ' ');
  const chars = Array.from(t);
  const a = Math.max(0, Math.min(Number(c.match_start) || 0, chars.length));
  const b = Math.max(a, Math.min(Number(c.match_end) || 0, chars.length));
  return esc(chars.slice(0, a).join('')) +
    (b > a ? `<mark class="wclip-hit">${esc(chars.slice(a, b).join(''))}</mark>` : '') +
    esc(chars.slice(b).join(''));
}

// loadWordClips 拉取该词原声；生成中的词轮询几次，面板收起即停。
async function loadWordClips(w, box, opts = {}) {
  if (!box) return;
  if (!box._stopBubble) {
    box._stopBubble = true;
    box.addEventListener('click', e => e.stopPropagation());
  }
  const cached = CLIP_MEM.get(w.id);
  if (cached && cached.length) { wclipRender(box, cached, opts, 0); return; }

  const title = opts.title || '原声台词';
  box.innerHTML = `<div class="wclip-head"><span class="usage-head">${title}</span></div><p class="hint">加载原声…</p>`;
  let tries = 0;
  const tick = async () => {
    if (!box.isConnected) return;
    try {
      const r = await api('/words/' + w.id + '/clips');
      if (!box.isConnected) return;
      const items = r.items || [];
      // 已缓存的片段即使搜索服务未配置也照常展示
      if (!r.enabled && !items.length) { box.innerHTML = ''; return; }
      if (items.length) { CLIP_MEM.set(w.id, items); wclipRender(box, items, opts, 0); return; }
      if (r.pending && tries < 20) {
        tries++;
        box.innerHTML = `<div class="wclip-head"><span class="usage-head">${title}</span></div>
          <p class="hint">原声例句生成中…（第一次遇到这个词，约需十几秒）</p>`;
        setTimeout(tick, 3000);
        return;
      }
      box.innerHTML = '';
    } catch {
      box.innerHTML = '';
    }
  };
  tick();
}

function wclipRender(box, clips, opts, offset) {
  clipStopAll();
  const title = opts.title || '原声台词';
  const total = clips.length;
  const n = Math.min(2, total);
  const shown = [];
  for (let i = 0; i < n; i++) shown.push(clips[(offset + i) % total]);

  let html = `<div class="wclip-head">
      <span class="usage-head">${title}</span>
      ${total > n ? '<button class="wclip-swap" data-act="swap">换两条 ↻</button>' : ''}
    </div>`;
  shown.forEach(c => {
    const src = [c.media_name || '原声',
      c.episode_title || (c.episode ? '第' + c.episode + '集' : '')].filter(Boolean).join(' · ');
    const zh = c.text_zh || c.text_en || '';
    const lang = c.zh_source === 'en' ? '<span class="wclip-lang">EN</span>' : '';
    html += `<div class="wclip-card">
      <div class="wclip-poster" role="button" tabindex="0" aria-label="播放原声">
        ${c.image_url ? `<img src="${esc(c.image_url)}" alt="" loading="lazy">` : ''}
        <span class="wclip-play">▶</span>
      </div>
      <div class="wclip-body">
        <div class="wclip-text">${clipTextHTML(c)}</div>
        ${zh ? `<div class="wclip-zh">${lang}${esc(zh)}</div>` : ''}
        <div class="wclip-meta">
          <span class="wclip-src">${esc(src)}</span>
          <button class="wclip-mini" data-act="audio" title="只听音频">🎧</button>
        </div>
      </div>
      ${c.audio_url ? `<audio class="wclip-audio" src="${esc(c.audio_url)}" preload="none"></audio>` : ''}
    </div>`;
  });
  box.innerHTML = html;

  box.querySelectorAll('.wclip-card').forEach(card => {
    const a = card.querySelector('.wclip-audio');
    const btn = card.querySelector('.wclip-mini');
    const poster = card.querySelector('.wclip-poster');
    const icon = poster && poster.querySelector('.wclip-play');
    if (!a) return;
    const toggle = () => {
      if (a.paused) {
        clipPauseOthers(a);
        a.play().catch(() => toast('音频播放失败'));
      } else a.pause();
    };
    if (btn) btn.onclick = (e) => { e.stopPropagation(); toggle(); };
    if (poster) {
      poster.onclick = (e) => { e.stopPropagation(); toggle(); };
      poster.onkeydown = (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); e.stopPropagation(); toggle(); }
      };
    }
    const setOn = (on) => {
      if (btn) btn.classList.toggle('on', on);
      if (poster) {
        poster.classList.toggle('on', on);
        poster.setAttribute('aria-label', on ? '暂停原声' : '播放原声');
      }
      if (icon) icon.textContent = on ? '⏸' : '▶';
    };
    a.addEventListener('play', () => setOn(true));
    a.addEventListener('pause', () => setOn(false));
    a.addEventListener('ended', () => setOn(false));
  });
  const swap = box.querySelector('.wclip-swap');
  if (swap) {
    swap.onclick = (e) => {
      e.stopPropagation();
      wclipRender(box, clips, opts, (offset + n) % total);
    };
  }
}

// renderPhrasesView 生词短语视图：每词一短语，点句即听（配音管线不变）
function renderPhrasesView() {
  $('mat-tabs').style.display = 'none';
  const pm = phrasesMaterial();
  const box = $('sents'); box.innerHTML = '';
  if (!pm) {
    $('tts-btn').style.display = 'none';
    box.innerHTML = `
      <div class="card" style="text-align:center">
        <h2>生词短语</h2>
        <p class="hint">为本课每个生词生成一个自然搭配短语（4—14 字符、省略主语的最小表达单位）——
        背搭配比背单词更接近「说」的最小单位（FR-2.15 搭配卡）。<br>
        生成后：点短语即听、可收藏、可连播。</p>
        <button class="btn accent" data-act="gen-phrases">为 ${LESSON.words.length} 个生词生成短语</button>
      </div>`;
    $('learn-hint').textContent = '尚未生成生词短语';
    return;
  }
  pm.sentences.forEach((s, idx) => {
    const has = (s.audios || []).length > 0;
    const fav = FAVS.has(s.id);
    const inRv = RV_POOL.has(s.id);
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(s.speaker || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
      <div class="zh">${esc(s.translation || '')}</div></div>
      <button class="rv-btn ${inRv ? 'on' : ''}" title="${inRv ? '已在复习池' : '加入复习'}">复</button>
      <button class="fav ${fav ? 'on' : ''}" title="${fav ? '取消收藏' : '收藏句子'}">${fav ? '★' : '☆'}</button>
      <button class="play ${has ? 'has' : ''}" title="${has ? '播放' : '未配音'}">${has ? '▶' : '…'}</button>`;
    div.querySelector('.rv-btn').onclick = (e) => { e.stopPropagation(); toggleReviewSentence(s, div); };
    div.querySelector('.fav').onclick = (e) => { e.stopPropagation(); toggleFavSentence(s, div); };
    div.onclick = () => playSentence(s, idx, div);
    box.appendChild(div);
  });
  applyMode();
  const withAudio = pm.sentences.filter(s => (s.audios || []).length).length;
  $('learn-hint').textContent = `生词短语 · 共 ${pm.sentences.length} 句 / 已配音 ${withAudio} 句 · 点短语即听 · 「▶ 整段连播」连播全部`;
  $('tts-btn').style.display = 'none';
}

// genPhrases 学习屏入口：为本课生词补生成短语包
async function genPhrases() {
  if (await genPhrasesCore(LESSON.lesson.id, LESSON.words || [])) {
    LESSON = await api('/lessons/' + LESSON.lesson.id);
    VIEW = 'phrases';
    go('learn'); renderPhrasesView();
  }
}

// genPhrasesCore 短语包生成核心（LLM → 校验 → mode=phrases 上传），返回成败。
// 统一从服务端拉课程词表（含 word_id，供 word_occurrence 重映射与校验）；
// 幂等前置：短语包已存在则直接使用（省一次 LLM 调用）；
// isRetry=true 表示已在自动重试（最多一次，避免无限消耗 token）。
async function genPhrasesCore(lessonID, words, isRetry) {
  const t0 = Date.now();
  go('gen');
  $('gen-retry').style.display = 'none'; $('gen-back').style.display = 'none';
  $('gen-log').style.display = 'none'; $('gen-log').textContent = '';
  $('gen-title').textContent = '生词短语生成中';
  $('gen-note').innerHTML = '为每个生词写一个自然短语，通常 1—2 分钟。';
  try {
    const full = await api('/lessons/' + lessonID);
    const svWords = full.words || [];
    if (!svWords.length) { genLog('本课无生词'); return true; }
    if ((full.materials || []).some(m => m.type === 'phrases')) {
      genLog('短语包已存在，直接使用');
      return true;
    }

    const prompt = buildPhrasesPrompt(svWords);
    genLog('调用大模型生成短语…');
    const started = { task_id: await startGen(lessonID, prompt) };
    saveActiveTask(started.task_id, lessonID, 'phrases');
    let llmText = '';
    while (true) {
      await new Promise(res2 => setTimeout(res2, 1000));
      let st;
      try { st = await pollGen(started.task_id); }
      catch (e2) { continue; }
      llmText = st.text || '';
      const sec = Math.floor(st.elapsed_ms / 1000);
      $('gen-step').textContent = `已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 已输出 ${st.chars || 0} 字符`;
      if (st.err) throw new Error('生成失败：' + st.err);
      if (st.done) break;
    }
    clearActiveTask();
    genLog(`生成完毕（${Math.floor((Date.now() - t0) / 1000)} 秒）`);
    const out = JSON.parse(largestJSON(llmText));

    // surface → word_id 重映射（与完整包同规则）
    const idBySurface = {};
    for (const w of svWords) idBySurface[w.surface] = w.id;
    const occ = {};
    for (const [k, v] of Object.entries(out.word_occurrence || {})) occ[idBySurface[k] || k] = v;
    out.word_occurrence = occ;

    // 短语包自修复：LLM 偶发漏写部分词的 word_occurrence——短语包每句的
    // speaker 就是词面，可直接从句子重建缺失的复现记录（无需重试 LLM）
    const pm = (out.materials || [])[0];
    if (pm && pm.sentences) {
      const bySpeaker = {};
      pm.sentences.forEach(s => { if (s.speaker) bySpeaker[s.speaker] = s.id; });
      let rebuilt = 0;
      for (const w of svWords) {
        const o = out.word_occurrence[w.id];
        const empty = !o || (Array.isArray(o) && o.length === 0);
        if (empty && bySpeaker[w.surface]) {
          out.word_occurrence[w.id] = [bySpeaker[w.surface]];
          rebuilt++;
        }
      }
      if (rebuilt > 0) genLog('自动重建了 ' + rebuilt + ' 个词的复现记录（模型漏写，已从短语修复）');
    }

    const problems = validateLocally(out, svWords, false);
    if (problems.length) {
      if (!isRetry) {
        genLog('首次输出未达标（' + problems.length + ' 项问题），自动重试一次…');
        return genPhrasesCore(lessonID, words, true);
      }
      throw new Error('短语校验未通过（已自动重试一次）：' + problems.join('；'));
    }

    try {
      await api(`/lessons/${lessonID}/materials`, {
        method: 'POST',
        body: JSON.stringify({ mode: 'phrases', ...sanitizeOutput(out) }),
      });
    } catch (e2) {
      if (!String(e2.message).includes('已生成')) throw e2; // 409 已存在 = 幂等成功
      genLog('短语包此前已生成，直接使用');
    }
    return true;
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-retry').style.display = 'block';
    $('gen-retry').textContent = '↻ 重新生成';
    $('gen-back').style.display = 'block';
    $('gen-back').textContent = '返回'; // 来源可能是学习屏或创建屏，按实际历史回退
    $('gen-back').onclick = goBack;
    $('gen-title').textContent = '短语生成失败';
    return false;
  }
}

// genFull 为短语模式课补生成句子模式材料（对话/短文）——学习屏「跟读」空态入口
async function genFull() {
  const words = LESSON.words || [];
  if (!words.length) return;
  const level = (LESSON.lesson && LESSON.lesson.level) || 'N4';
  const t0 = Date.now();
  go('gen');
  $('gen-retry').style.display = 'none'; $('gen-back').style.display = 'none';
  $('gen-log').style.display = 'none'; $('gen-log').textContent = '';
  $('gen-title').textContent = '句子模式材料生成中';
  $('gen-note').innerHTML = '生成对话与短文（约 3—5 分钟），完成后这节课两种模式齐备。';
  try {
    const prompt = buildPrompt(words, level);
    genLog('调用大模型生成对话/短文…');
    const started = { task_id: await startGen(LESSON.lesson.id, prompt) };
    saveActiveTask(started.task_id, LESSON.lesson.id, 'full');
    let llmText = '';
    while (true) {
      await new Promise(res2 => setTimeout(res2, 1000));
      let st;
      try { st = await pollGen(started.task_id); }
      catch (e2) { continue; }
      llmText = st.text || '';
      const sec = Math.floor(st.elapsed_ms / 1000);
      $('gen-step').textContent = `已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 已输出 ${st.chars || 0} 字符`;
      if (st.err) throw new Error('生成失败：' + st.err);
      if (st.done) break;
    }
    clearActiveTask();
    genLog(`生成完毕（${Math.floor((Date.now() - t0) / 1000)} 秒）`);
    const out = JSON.parse(largestJSON(llmText));
    const idBySurface = {};
    for (const w of words) idBySurface[w.surface] = w.id;
    const occ = {};
    for (const [k, v] of Object.entries(out.word_occurrence || {})) occ[idBySurface[k] || k] = v;
    out.word_occurrence = occ;
    const problems = validateLocally(out, words, true);
    if (problems.length) throw new Error('材料校验未通过：' + problems.join('；'));
    try {
      await api(`/lessons/${LESSON.lesson.id}/materials`, {
        method: 'POST',
        body: JSON.stringify(sanitizeOutput(out)),
      });
    } catch (e2) {
      if (!String(e2.message).includes('已生成')) throw e2; // 409 已存在 = 幂等成功
      genLog('对话/短文此前已生成，直接使用');
    }
    genLog('落库成功');
    LESSON = await api('/lessons/' + LESSON.lesson.id);
    VIEW = 'material'; CUR_MAT = 0;
    go('learn'); renderLearn();
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-retry').style.display = 'block';
    $('gen-retry').textContent = '↻ 重新生成';
    $('gen-back').style.display = 'block';
    $('gen-back').textContent = '返回学习';
    $('gen-back').onclick = goBack;
    $('gen-title').textContent = '生成失败';
  }
}

// buildPhrasesPrompt 生词短语包的生成提示（FR-2.15 搭配卡形态）
function buildPhrasesPrompt(words) {
  const lines = words.map((w, i) => `p${i + 1}｜${w.surface}｜${w.reading || '—'}｜${w.meaning}`);
  return `你是日语教学专家。请为下面的每个词生成**恰好一个搭配短语**（フレーズ），不是完整句子。

短语 = 该词最典型的最小使用单位：省略主语、省略时间/场景铺垫，只保留自然的词语搭配，4—14 个日文字符。
  好的例子：切符を買う ／ 切符を二枚ください ／ 電子切符にする ／ 部屋を掃除する
  坏的例子：京都行きの切符を昨日買いました。（完整叙述句：加了时间背景，太长，不要这样写）

要求：
- 每个短语必须包含该词本身；名词短语或です・ます体短语皆可
- 严格控制长度：超过 14 个字符说明写成句子了，请缩短重写
- 句子 id 用 p1、p2…（与词汇表顺序对应），sort_order 从 1 开始
- 每句包含 text（日语短语）、kana（整句假名读音）、translation（中文翻译）、speaker（填该词的 surface）
- word_occurrence：key 是词的 surface，value 是该词所在短语的 id 数组（如 ["p1"]）

词汇表（id｜surface｜读音｜中文）：
${lines.join('\n')}

严格按如下 JSON 结构输出（不要 markdown 围栏、不要解释）：
{"materials":[{"id":"ph1","type":"phrases","scene":"生词短语","title":"生词短语","role_a":null,"role_b":null,"sort_order":99,"sentences":[{"id":"p1","sort_order":1,"speaker":"精算","text":"...","kana":"...","translation":"...","para_no":0}]}],"qa_items":[],"substitution_items":[],"word_occurrence":{"精算":["p1"]}}`;
}
// ============ 生成任务状态（跨页面可见：离开生成屏后首页可查、可恢复） ============
function saveActiveTask(taskID, lessonID, mode) {
  localStorage.setItem('ts_task', JSON.stringify({
    task_id: taskID, lesson_id: lessonID, mode, started: Date.now(),
  }));
}
function getActiveTask() {
  try { return JSON.parse(localStorage.getItem('ts_task')); } catch { return null; }
}
function clearActiveTask() { localStorage.removeItem('ts_task'); }

// checkActiveTask 首页状态卡：进行中显示进度（点击回生成屏），完成显示结果入口
async function checkActiveTask() {
  const t = getActiveTask();
  const card = $('gen-status-card');
  if (!t) { card.style.display = 'none'; return; }
  let st;
  try {
    st = await pollGen(t.task_id);
  } catch (e) {
    card.style.display = 'none'; return; // 任务不可查（服务重启等）——静默隐藏
  }
  card.style.display = 'block';
  const label = t.mode === 'phrases' ? '生词短语' : '对话/短文';
  const btn = $('gen-status-btn');
  if (st.done && !st.err) {
    $('gen-status-text').innerHTML = `「${label}」✅ 生成完成（${st.chars} 字符）`;
    btn.textContent = '查看结果';
    btn.onclick = async () => {
      clearActiveTask();
      LESSON = await api('/lessons/' + t.lesson_id);
      if (t.mode === 'phrases') { VIEW = 'phrases'; go('learn'); renderPhrasesView(); }
      else { VIEW = 'material'; CUR_MAT = 0; go('learn'); renderLearn(); }
    };
  } else if (st.err) {
    $('gen-status-text').textContent = `「${label}」生成失败：${st.err}`;
    btn.textContent = '返回重试';
    btn.onclick = () => { clearActiveTask(); go('create'); };
  } else {
    const sec = Math.floor(st.elapsed_ms / 1000);
    $('gen-status-text').innerHTML =
      `「${label}」生成中 · 已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 已输出 ${st.chars} 字符`;
    btn.textContent = '查看进度';
    btn.onclick = () => resumeActiveGen();
  }
}

// resumeActiveGen 从首页回到生成屏，恢复轮询直到完成并跳转对应视图
async function resumeActiveGen() {
  const t = getActiveTask();
  if (!t) return;
  go('gen');
  $('gen-retry').style.display = 'none'; $('gen-back').style.display = 'none';
  $('gen-title').textContent = '材料生成中（已恢复进度）';
  $('gen-preview').style.display = 'block';
  try {
    let llmText = '';
    while (true) {
      await new Promise(r => setTimeout(r, 1000));
      let st;
      try { st = await pollGen(t.task_id); }
      catch (e) { continue; }
      llmText = st.text || '';
      const sec = Math.floor(st.elapsed_ms / 1000);
      if (st.chars > 0) $('gen-preview').textContent = '…' + llmText.slice(-150).replace(/\s+/g, ' ');
      $('gen-step').textContent = `已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 已输出 ${st.chars} 字符`;
      if (st.err) throw new Error(st.err);
      if (st.done) break;
    }
    clearActiveTask();
    $('gen-preview').textContent = '';
    const out = JSON.parse(largestJSON(llmText));
    const full = await api('/lessons/' + t.lesson_id);
    const idBySurface = {};
    for (const w of full.words) idBySurface[w.surface] = w.id;
    const occ = {};
    for (const [k, v] of Object.entries(out.word_occurrence || {})) occ[idBySurface[k] || k] = v;
    out.word_occurrence = occ;
    const problems = validateLocally(out, full.words, t.mode !== 'phrases');
    if (problems.length) throw new Error('材料校验未通过：' + problems.join('；'));
    try {
      await api(`/lessons/${t.lesson_id}/materials`, {
        method: 'POST',
        body: JSON.stringify(t.mode === 'phrases' ? { mode: 'phrases', ...sanitizeOutput(out) } : sanitizeOutput(out)),
      });
    } catch (e2) { if (!String(e2.message).includes('已生成')) throw e2; }
    LESSON = await api('/lessons/' + t.lesson_id);
    if (t.mode === 'phrases') { VIEW = 'phrases'; go('learn'); renderPhrasesView(); }
    else { VIEW = 'material'; CUR_MAT = 0; go('learn'); renderLearn(); }
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-title').textContent = '生成失败';
    $('gen-back').style.display = 'block';
    $('gen-back').textContent = '返回首页';
    $('gen-back').onclick = () => { clearActiveTask(); goBack(); };
  }
}

// loadHome 回到首页枢纽：今日状态 + 历史课程入口（学习屏的返回落点）
async function loadHome() {
  if (!TOKEN) { go('login'); return; } // 未登录防御：绝不停留在需鉴权的屏
  const today = new Date().toISOString().slice(0, 10);
  const heroDate = (d) => d.slice(5).replace('-', '.');
  let lessons = [];
  try {
    const list = await api('/lessons');
    lessons = list.lessons || [];
  } catch (e) {
    showErr('home-err', e.message); go('home'); return;
  }
  checkActiveTask(); // 首页显示进行中/已完成的生成任务（可点入）
  loadReviewCount(); // 首页显示到期复习数量（不阻塞主流程）
  loadWordStudyStatus(); // 首页背词卡片状态（不阻塞主流程）
  loadLibCount();    // 首页显示生词库数量（不阻塞主流程）
  loadNbCount();     // 首页显示生词本概览（不阻塞主流程）
  loadRecCount();    // 首页显示课文本数量（不阻塞主流程）
  updateCacheStats(); // 首页本地音频缓存占用（不阻塞主流程）

  const chip = $('lesson-chip'), btn = $('home-main-btn');
  const todayIt = lessons.find(x => x.date === today);
  const openLesson = (id) => async () => {
    LESSON = await api('/lessons/' + id);
    VIEW = 'material'; CUR_MAT = 0;
    go('learn'); renderLearn();
  };

  if (todayIt && todayIt.status === 'ready') {
    chip.textContent = '可学习'; chip.classList.add('ready');
    $('lesson-date').textContent = heroDate(todayIt.date);
    $('lesson-date').title = todayIt.date;
    $('lesson-desc').textContent = `今日课程已就绪（${todayIt.word_count} 词）`;
    btn.textContent = '▶ 继续学习';
    btn.onclick = openLesson(todayIt.lesson_id);
    // 今日已生成时保留「新建词单」入口（重做今日：确认后删课重建）
    $('home-new-btn').style.display = 'block';
  } else if (todayIt) {
    chip.textContent = '准备中'; chip.classList.remove('ready');
    $('lesson-date').textContent = heroDate(todayIt.date);
    $('lesson-date').title = todayIt.date;
    $('lesson-desc').textContent = '今日课程草稿就绪，可继续提交词单生成。';
    btn.textContent = '继续提交词单';
    btn.onclick = () => go('create');
    $('home-new-btn').style.display = 'none';
  } else {
    chip.textContent = '未创建'; chip.classList.remove('ready');
    $('lesson-date').textContent = heroDate(today);
    $('lesson-date').title = today;
    $('lesson-desc').textContent = '还没有今天的课程。1 个词也可以生成一课。';
    btn.textContent = '创建今日课程';
    btn.onclick = () => go('create');
    $('home-new-btn').style.display = 'none';
  }

  // 历史课程（ready 可进学习屏：重听/收藏）
  const readyOnes = lessons.filter(x => x.status === 'ready').sort((a, b) => b.date.localeCompare(a.date));
  const hl = $('history-list'); hl.innerHTML = '';
  if (!readyOnes.length) {
    hl.innerHTML = '<p class="hint">暂无已完成的历史课程。</p>';
  } else {
    readyOnes.slice(0, 10).forEach(x => {
      const b = document.createElement('button');
      b.className = 'btn ghost';
      b.style.marginTop = '6px';
      b.textContent = `${x.date} · ${x.word_count} 词 · 打开学习`;
      b.onclick = openLesson(x.lesson_id);
      hl.appendChild(b);
    });
  }
  go('home');
}

// ============ 词单 ============
// IME 安全处理（中日输入法关键）：composition（组词）期间不触发解析——
// 组合中的假名/候选文字不是最终输入，边组词边解析会与 IME 内部状态互相干扰导致卡顿；
// compositionend（用户按回车/空格确认候选）后才解析。另有 150ms 节流。
let composing = false, countTimer = null;
function countWords() {
  const n = parseWords($('words-input').value).length;
  let tip = '';
  if (n === 0) tip = '（还没输入）';
  else if (n === 1) tip = '（一个词也可以生成）';
  else if (n < 5) tip = '（词少也可以生成）';
  else if (n > 40) tip = '（词较多，生成与学习时间会变长）';
  $('words-count').textContent = n + ' 词' + tip;
}
function scheduleCount() {
  if (composing) return;
  if (countTimer) clearTimeout(countTimer);
  countTimer = setTimeout(countWords, 150);
}
{
  const wi = $('words-input');
  wi.addEventListener('compositionstart', () => (composing = true));
  wi.addEventListener('compositionend', () => { composing = false; scheduleCount(); });
  wi.addEventListener('input', scheduleCount);
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

// ============ 生成模式选择 ============
let GEN_MODE = 'full'; // full=句子模式（对话+短文） / phrases=短语模式（每词一短语）
function pickGenMode(m) {
  GEN_MODE = m;
  $('mode-full').classList.toggle('on', m === 'full');
  $('mode-phrases').classList.toggle('on', m === 'phrases');
  $('mode-desc').textContent = m === 'full'
    ? '对话 + 短文：每个词自然出现在有场景的完整句子里（约 3—5 分钟）'
    : '每个生词一个搭配短语（3—10 词短句，约 1—2 分钟）——背搭配比背单词更接近「说」';
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
  $('gen-retry').style.display = 'none'; $('gen-back').style.display = 'none';
  $('gen-log').style.display = 'none'; $('gen-log').textContent = '';
  $('gen-title').textContent = '材料生成中';
  $('gen-note').innerHTML = '正在用大模型围绕你的词单写对话和短文，通常需要 2—5 分钟。<br>请不要关闭本页。';
  go('gen');
  try {
    // 1. 建课/复用（同日词单一致 → 直接复用；仅换了词单才确认删课重做）
    const today = new Date().toISOString().slice(0, 10);
    const buildBody = () => JSON.stringify({
      lesson_date: today, words,
      params: { scenes: ['便利店', '车站', '家'], level: $('sel-level').value },
    });
    let lessonID;
    try {
      const r = await api('/lessons', { method: 'POST', body: buildBody() });
      lessonID = r.lesson_id;
      genLog('课程就绪 ' + lessonID);
    } catch (e) {
      if (!String(e.message).includes('已存在')) throw e;
      const list = await api(`/lessons?from=${today}&to=${today}`);
      const existing = (list.lessons || [])[0];
      const full = await api('/lessons/' + existing.lesson_id);
      const oldSet = new Set(full.words.map(w => w.surface));
      const newSet = new Set(words.map(w => w.surface));
      const sameWords = oldSet.size === newSet.size && [...newSet].every(s => oldSet.has(s));
      if (!sameWords && existing.status === 'ready') {
        // 只有换了词单才需要删除重做（课程与词单绑定）
        if (!confirm('今日课程的词单与本次提交不同。\n\n重新提交将删除今日课程并按新词单重新生成。\n（已生成的句子配音不受影响：重建后相同句子直接命中缓存，零重复计费）\n\n确定重做？')) {
          throw new Error('已取消');
        }
        genLog('词单已变更，删除今日课程（按新词单重建）…');
        await api('/lessons/' + existing.lesson_id, { method: 'DELETE' });
        const r2 = await api('/lessons', { method: 'POST', body: buildBody() });
        lessonID = r2.lesson_id;
      } else {
        lessonID = existing.lesson_id; // 词单一致或 draft：复用现有课程
        genLog('词单与今日课程一致，复用现有课程');
      }
    }

    // 按所选模式分流：短语模式走短语包（快速），句子模式走完整包（现有）
    if (GEN_MODE === 'phrases') {
      const okk = await genPhrasesCore(lessonID, words);
      if (!okk) return; // Core 已在生成屏显示真实错误与重试按钮，勿覆盖
      genLog('短语包落库成功');
      LESSON = await api('/lessons/' + lessonID);
      VIEW = 'phrases';
      go('learn'); renderPhrasesView();
      return;
    }

    // 2. LLM 生成材料（校验未通过自动重试一次；后台任务 + 轮询不丢进度）
    let out = null, lastProblems = [];
    for (let attempt = 1; attempt <= 2 && !out; attempt++) {
      if (attempt === 2) {
        genLog('首次输出未达标（' + lastProblems.length + ' 项问题），自动重试一次…');
        $('gen-title').textContent = '材料生成中（自动重试）';
      }
      const prompt = buildPrompt(words, $('sel-level').value);
      genLog(attempt === 1 ? '调用大模型生成材料…' : '重试：调用大模型生成材料…');
      let llmText = '', fullLen = 0, llmErr = '';
      const t0 = Date.now();
      $('gen-preview').style.display = 'block';
      const taskID = await startGen(lessonID, prompt);
      saveActiveTask(taskID, lessonID, 'full');
      genLog('生成任务已启动（服务端后台执行，刷新页面也不会中断）');
      // 轮询循环
      while (true) {
        await new Promise(r => setTimeout(r, 1000));
        let st;
        try {
          st = await pollGen(taskID);
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
      $('gen-step').textContent = `生成完成（${fullLen} 字符 / ${genSec} 秒），正在校验…`;
      const cand = JSON.parse(largestJSON(llmText));
      genLog('材料解析成功（' + (cand.materials || []).length + ' 段）');

      // 3. 校验 + surface → word_id 重映射
      const full = await api('/lessons/' + lessonID);
      const idBySurface = {};
      for (const w of full.words) idBySurface[w.surface] = w.id;
      const occ = {};
      for (const [k, v] of Object.entries(cand.word_occurrence || {})) occ[idBySurface[k] || k] = v;
      cand.word_occurrence = occ;
      const problems = validateLocally(cand, full.words, true);
      if (!problems.length) { out = cand; } else { lastProblems = problems; }
    }
    if (!out) throw new Error('材料校验未通过（已自动重试一次）：' + lastProblems.join('；'));
    $('gen-preview').textContent = '';
    clearActiveTask();
    genLog('校验通过，上传落库…');

    // 4. 上传（白名单净化后的干净结构；同材料已生成 = 幂等成功直接使用）
    try {
      const up = await api(`/lessons/${lessonID}/materials`, { method: 'POST', body: JSON.stringify(sanitizeOutput(out)) });
      genLog('落库成功 status=' + up.status + (up.warnings?.length ? '（注音警告 ' + up.warnings.length + ' 句）' : ''));
    } catch (e2) {
      if (!String(e2.message).includes('已生成')) throw e2;
      genLog('对话/短文此前已生成，直接使用');
    }

    LESSON = await api('/lessons/' + lessonID);
    go('learn'); renderLearn();
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-retry').style.display = 'block';
    $('gen-back').style.display = 'block';
    $('gen-title').textContent = '生成失败';
  }
}

// 白名单净化：只保留 schema 字段上传（LLM 会附加 "topic" 等多余字段，
// 且杜绝对未知字段的敏感——防御深度）
function sanitizeOutput(out) {
  return {
    materials: (out.materials || []).map(m => ({
      id: m.id, type: m.type, scene: m.scene || '', title: m.title || '',
      role_a: m.role_a ?? null, role_b: m.role_b ?? null,
      sort_order: m.sort_order || 0,
      sentences: (m.sentences || []).map(s => ({
        id: s.id, sort_order: s.sort_order || 0, speaker: s.speaker || null,
        text: s.text || '', kana: s.kana || '', translation: s.translation || '',
        para_no: s.para_no || 0,
      })),
    })),
    qa_items: (out.qa_items || []).map(q => ({
      prompt: q.prompt || '', answer: q.answer || '', prompt_translation: q.prompt_translation || '',
    })),
    substitution_items: (out.substitution_items || []).map(s => ({
      word_id: s.word_id || '', prompt_zh: s.prompt_zh || '', answer: s.answer || '',
    })),
    word_occurrence: out.word_occurrence || {},
  };
}

// levelHint 水平的具体化描述（抽象指令 → 可执行标准，降低模型自由发挥）
function levelHint(level) {
  switch (level) {
    case 'N5': return '只用最基础的语法（です/ます/あります/基本动词て形），词汇尽量控制在 N5 表内';
    case 'N4': return '可用 N4 语法（て形连接、被动、可能形等简单句式），避免长难句';
    case 'N3': return '可用 N3 语法与相对丰富的表达，仍以口语常用句为主';
    default: return '只用最基础的语法与词汇';
  }
}

function validateLocally(out, words, strict) {
  const problems = [];
  const sids = new Set();
  for (const m of out.materials || []) {
    for (const s of m.sentences || []) sids.add(s.id);
    const n = (m.sentences || []).length;
    if (m.type === 'phrases') {
      if (n < 1 || n > 40) problems.push(`短语材料 ${m.id} 句数 ${n} 异常`);
      // 短语超长 = 模型写成了完整句子（prompt 已给正反例，仍需硬校验兜底）
      for (const s of m.sentences || []) {
        const len = (s.text || '').replace(/[、。！？\s「」]/g, '').length;
        if (len > 14) problems.push(`短语「${s.text}」${len} 字符过长，疑似完整句子`);
      }
    } else if (strict && (n < 8 || n > 12)) {
      problems.push(`材料 ${m.id} 句数 ${n} 超出 8—12`);
    }
  }
  for (const w of words) {
    const occ = (out.word_occurrence || {})[w.id];
    if (!occ) { problems.push(`词 ${w.surface} 未在复现表交代`); continue; }
    const list = Array.isArray(occ) ? occ : (occ.sentences || []);
    const skipped = typeof occ === 'string' && occ.startsWith('skipped');
    if (!skipped && list.length === 0) problems.push(`词 ${w.surface} 复现列表为空`);
    if (strict && !skipped && w.is_key && list.length < 2) {
      problems.push(`重点词 ${w.surface} 仅出现 1 次（需 2 次）`);
    }
  }
  return problems;
}

function buildPrompt(words, level) {
  const lines = words.map(w => `${w.surface}｜${w.reading || '—'}｜${w.meaning}${w.is_key ? '｜★重点：出现 2 次' : ''}`);
  return `你是日语听说学习材料的编写专家。请为下面的词汇表生成一份完整的 JSON 材料包。

要求：
- 难度严格贴合 JLPT ${level || 'N5'} 水平：${levelHint(level)}；全部句子用です・ます体，自然、贴近日常生活
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
// 数字（半角/全角/汉字）与量词在视觉上连成一个整体，但不并成一个 ruby 词块，
// 保证振り仮名只盖在汉字量词上（3年生 → 3 と 年生）。
const NUM_CH = /[0-9０-９〇零一二三四五六七八九十百千万億兆]/;
function isNumTok(w) {
  return !!w && Array.from(w).every(c => NUM_CH.test(c));
}
const COUNTER_HEAD = [
  'ヶ月', 'ヵ月', 'か月', 'カ月', '種類', '年生', '週間', '時間', '分間', '日間', '年間',
  'ヶ所', '箇所', 'ページ', '文字',
  'キロメートル', 'センチメートル', 'ミリメートル', 'キログラム', 'ミリリットル',
  'メートル', 'センチ', 'ミリ', 'グラム', 'キロ', 'リットル', 'ヘクタール', 'パーセント',
  '年', '月', '日', '時', '分', '秒', '階', '歳', '才', '人', '名', '枚', '個', '本',
  '杯', '匹', '台', '冊', '円', '回', '度', '番', '号', '件', '曲', '話', '軒', '泊',
  '着', '足', '頭', '羽', '組', 'つ', '点', '倍', '割', 'ドル', 'ユーロ', '元',
];
// 日期/时刻的下一段允许继续连（3月20日、10時30分）
const DATE_JOIN = new Set(['月', '日', '年', '時', '分', '秒']);
function isCounterTok(w) {
  return !!w && COUNTER_HEAD.some(h => w.startsWith(h));
}
function keepsNumRun(w) {
  return isNumTok(w) || (isCounterTok(w) && DATE_JOIN.has(Array.from(w)[0]));
}
// force=true 时单个词也分词（消失术等逐词渲染）；'plain' 用于词条本身不分词
function rubyHTML(tokens, force) {
  if (!Array.isArray(tokens) || !tokens.length) return '';
  const seg = force === 'plain' ? false
    : (force || tokens.filter(([w]) => w && !recIsPunct(w)).length >= 2);
  let first = true;
  let numRun = false;
  return tokens
    .map(([w, r]) => {
      const inner = (r && /[一-龯]/.test(w)) ? `<ruby>${esc(w)}<rt>${esc(r)}</rt></ruby>` : esc(w);
      if (!seg || !w || recIsPunct(w)) { numRun = false; return inner; }
      const join = !first && numRun && (isNumTok(w) || isCounterTok(w));
      const cls = first ? 'wseg wseg-first' : (join ? 'wseg wseg-join' : 'wseg');
      first = false;
      numRun = keepsNumRun(w);
      return `<span class="${cls}">${inner}</span>`;
    })
    .join('');
}

function renderLearn() {
  if (VIEW === 'fav') return renderFavView();
  if (VIEW === 'phrases') return renderPhrasesView();
  // 生词短语在独立视图展示，不混入对话/短文 tabs
  const mats = (LESSON.materials || []).filter(m => m.type !== 'phrases');
  const tabs = $('mat-tabs'); tabs.innerHTML = '';
  tabs.style.display = 'flex';
  if (!mats.length) {
    // 短语模式课：句子模式材料未生成——提供按需补生成入口
    $('sents').innerHTML = `
      <div class="card" style="text-align:center">
        <h2>句子模式材料</h2>
        <p class="hint">这门课是「短语模式」生成的，还没有对话/短文。<br>
        每个词自然出现在有场景的完整句子里——可按需补生成（约 3—5 分钟）。</p>
        <button class="btn accent" data-act="gen-full">生成对话与短文</button>
      </div>`;
    applyMode();
    $('learn-hint').textContent = '句子模式材料未生成';
    $('tts-btn').style.display = 'none';
    return;
  }
  mats.forEach((m, i) => {
    const fav = FAVM.has(m.id);
    const b = document.createElement('button');
    b.className = i === CUR_MAT ? 'on' : '';
    b.innerHTML = (fav ? '★ ' : '') + esc(m.title || m.scene || ('材料 ' + (i + 1)));
    b.onclick = () => { VIEW = 'material'; CUR_MAT = i; renderLearn(); };
    const star = document.createElement('span');
    star.className = 'tab-star' + (fav ? ' on' : '');
    star.textContent = fav ? '★' : '☆';
    star.title = fav ? '取消收藏材料' : '收藏本材料';
    star.onclick = (e) => { e.stopPropagation(); toggleFavMaterial(m.id); };
    b.appendChild(star);
    tabs.appendChild(b);
  });
  const m = mats[CUR_MAT] || { sentences: [] };
  const box = $('sents'); box.innerHTML = '';
  m.sentences.forEach((s, idx) => {
    const has = (s.audios || []).length > 0;
    const fav = FAVS.has(s.id);
    const inRv = RV_POOL.has(s.id);
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(s.speaker || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
      <div class="zh">${esc(s.translation || '')}</div></div>
      <button class="rv-btn ${inRv ? 'on' : ''}" title="${inRv ? '已在复习池' : '加入复习'}">${inRv ? '复' : '复'}</button>
      <button class="fav ${fav ? 'on' : ''}" title="${fav ? '取消收藏' : '收藏句子'}">${fav ? '★' : '☆'}</button>
      <button class="play ${has ? 'has' : ''}" title="${has ? '播放' : '未配音'}">${has ? '▶' : '…'}</button>`;
    div.querySelector('.rv-btn').onclick = (e) => { e.stopPropagation(); toggleReviewSentence(s, div); };
    div.querySelector('.fav').onclick = (e) => { e.stopPropagation(); toggleFavSentence(s, div); };
    div.onclick = () => playSentence(s, idx, div);
    box.appendChild(div);
  });
  applyMode();
  updateLearnHint();
}

// renderFavView 收藏视图：全部收藏句（可点播/连播，未配音的自动现场配音）
function renderFavView() {
  $('mat-tabs').style.display = 'none';
  $('tts-btn').style.display = 'none';
  const box = $('sents'); box.innerHTML = '';
  const items = FAV_ITEMS || [];
  if (!items.length) {
    box.innerHTML = '<p class="hint" style="text-align:center;margin-top:30px">还没有收藏句子。<br>回到「跟读」视图，点句子旁的 ☆ 即可收藏。</p>';
    $('learn-hint').textContent = `★ 收藏 · 共 0 句`;
    return;
  }
  items.forEach((it, idx) => {
    const s = {
      id: it.sentence_id, text: it.text, ruby_tokens: it.ruby_tokens,
      translation: it.translation,
      audios: it.audio_id ? [{ id: it.audio_id }] : [],
      _fav: true,
    };
    const inRv = RV_POOL.has(s.id);
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(it.lesson_date || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(it.ruby_tokens) || esc(it.text)}</div>
      <div class="zh">${esc(it.translation || '')}</div></div>
      <button class="rv-btn ${inRv ? 'on' : ''}" title="${inRv ? '已在复习池' : '加入复习'}">复</button>
      <button class="fav on" title="取消收藏">★</button>
      <button class="play ${it.audio_id ? 'has' : ''}">${it.audio_id ? '▶' : '…'}</button>`;
    div.querySelector('.rv-btn').onclick = (e) => { e.stopPropagation(); toggleReviewSentence(s, div); };
    div.querySelector('.fav').onclick = (e) => { e.stopPropagation(); toggleFavSentence(s, div); };
    div.onclick = () => playSentence(s, idx, div);
    box.appendChild(div);
  });
  applyMode();
  $('learn-hint').textContent = `★ 收藏 · 共 ${items.length} 句 · 点句子即听 · 「▶ 整段连播」连播全部收藏`;
}

// toggleFavSentence 收藏/取消收藏句子（星标局部更新，不重建列表）
async function toggleFavSentence(s, div) {
  try {
    if (FAVS.has(s.id)) {
      await api('/favorites/sentences/' + s.id, { method: 'DELETE' });
      FAVS.delete(s.id);
      FAV_ITEMS = FAV_ITEMS.filter(x => x.sentence_id !== s.id);
    } else {
      await api('/favorites', { method: 'POST', body: JSON.stringify({ sentence_id: s.id }) });
      FAVS.add(s.id);
      FAV_ITEMS.unshift({
        sentence_id: s.id, text: s.text, ruby_tokens: s.ruby_tokens,
        translation: s.translation, audio_id: (s.audios || [])[0]?.id || null,
        lesson_date: (LESSON && LESSON.lesson && LESSON.lesson.date) || '',
      });
    }
    if (div) {
      const btn = div.querySelector('.fav');
      const on = FAVS.has(s.id);
      btn.textContent = on ? '★' : '☆';
      btn.classList.toggle('on', on);
    }
  } catch (e) { alert(e.message); }
}

// toggleFavMaterial 收藏/取消收藏材料（tab 星标刷新）
async function toggleFavMaterial(mid) {
  try {
    const r = await api('/favorites/materials', { method: 'POST', body: JSON.stringify({ material_id: mid }) });
    (r.favorited ? FAVM.add(mid) : FAVM.delete(mid));
    renderLearn();
  } catch (e) { alert(e.message); }
}

// toggleReviewSentence 加入/移出复习池（局部更新按钮状态）
async function toggleReviewSentence(s, div) {
  try {
    if (RV_POOL.has(s.id)) {
      // 已在池中 → 标记已掌握（移出活跃池）
      // 需要找到 review_item id，这里简化处理：直接调用 master 需要 id
      // 先用 due 列表找 id（仅活跃项），若已掌握则跳过
      const due = await api('/reviews/due');
      const it = (due.items || []).find(x => x.sentence_id === s.id);
      if (it) {
        await api('/reviews/' + it.id + '/master', { method: 'POST' });
      }
      RV_POOL.delete(s.id);
    } else {
      await api('/reviews', {
        method: 'POST',
        body: JSON.stringify({ sentence_id: s.id, source: 'manual' }),
      });
      RV_POOL.add(s.id);
    }
    if (div) {
      const btn = div.querySelector('.rv-btn');
      const on = RV_POOL.has(s.id);
      btn.classList.toggle('on', on);
      btn.title = on ? '已在复习池' : '加入复习';
    }
  } catch (e) { alert(e.message); }
}

// updateLearnHint 只刷新底部统计与配音按钮显隐（不碰句子 DOM，模式状态安全）
function updateLearnHint() {
  const mats = LESSON.materials || [];
  const total = mats.reduce((n, x) => n + x.sentences.length, 0);
  const withAudio = mats.reduce((n, x) => n + x.sentences.filter(s => (s.audios || []).length).length, 0);
  $('learn-hint').textContent = `共 ${total} 句 / 已配音 ${withAudio} 句 · 点句子即听（首次自动配音） · 顶部切换模式`;
  const btn = $('tts-btn');
  btn.style.display = withAudio < total ? 'block' : 'none';
  if (btn.style.display === 'block') btn.textContent = '为全部句子配音';
}

// refreshSentenceButtons 局部同步播放按钮三态（不重建列表，模式/译文/高亮不丢）
function refreshSentenceButtons() {
  const sents = (LESSON.materials[CUR_MAT] || {}).sentences || [];
  sents.forEach((s, i) => {
    const div = document.querySelector(`.sent[data-idx="${i}"]`);
    if (!div) return;
    const btn = div.querySelector('.play');
    const has = (s.audios || []).length > 0;
    btn.textContent = has ? '▶' : '…';
    btn.classList.toggle('has', has);
    btn.classList.remove('busy');
  });
  updateLearnHint();
}

// applyMode 把当前模式（盲听模糊/译文展开）幂等地应用到 DOM（重渲染后恢复用）
function applyMode() {
  const blind = CUR_MODE === 'blind', zh = CUR_MODE === 'zh';
  document.querySelectorAll('#sents .txt').forEach(el => (el.style.filter = blind ? 'blur(6px)' : 'none'));
  document.querySelectorAll('#sents .sent').forEach(el => el.classList.toggle('showzh', zh));
}

function setMode(mode) {
  CUR_MODE = mode;
  applyMode();
  $('md-normal').classList.toggle('on', mode === 'normal');
  $('md-blind').classList.toggle('on', mode === 'blind');
  $('md-zh').classList.toggle('on', mode === 'zh');
}
function toggleRT() {
  const off = document.body.classList.toggle('no-rt');
  $('md-rt').classList.toggle('on', !off);
}

// audioURL 取句子音频并返回同源 blob URL（服务端流式代理返回 MP3，
// 302/跨域/预签名全部绕开；Audio 元素无法带 Bearer，故先 fetch 成 blob）
// ============ 本地音频缓存（IndexedDB，FR-4.6 离线/省流量）============
// 命中零网络；未命中拉取后落缓存。重复听/连播/复习再听都不再下载。
const AUDIO_DB = 'tingshuo-audio', AUDIO_STORE = 'audio';
const AUDIO_MAX_BYTES = 150 * 1024 * 1024; // 上限 150MB，超出按最久未用淘汰
let _adb = null;
function idbOpen() {
  if (_adb) return Promise.resolve(_adb);
  return new Promise((res, rej) => {
    const rq = indexedDB.open(AUDIO_DB, 1);
    rq.onupgradeneeded = () => {
      const db = rq.result;
      if (!db.objectStoreNames.contains(AUDIO_STORE)) {
        db.createObjectStore(AUDIO_STORE, { keyPath: 'id' }).createIndex('lastUsed', 'lastUsed');
      }
    };
    rq.onsuccess = () => { _adb = rq.result; res(_adb); };
    rq.onerror = () => rej(rq.error);
  });
}
function _idbAll() {
  return idbOpen().then(db => new Promise(res => {
    const rq = db.transaction(AUDIO_STORE).objectStore(AUDIO_STORE).getAll();
    rq.onsuccess = () => res(rq.result || []); rq.onerror = () => res([]);
  }));
}
async function idbGet(id) {
  try {
    const db = await idbOpen();
    return await new Promise(res => {
      const rq = db.transaction(AUDIO_STORE).objectStore(AUDIO_STORE).get(id);
      rq.onsuccess = () => res(rq.result || null); rq.onerror = () => res(null);
    });
  } catch { return null; }
}
async function idbDel(id) {
  try {
    const db = await idbOpen();
    await new Promise(res => {
      const tx = db.transaction(AUDIO_STORE, 'readwrite');
      tx.objectStore(AUDIO_STORE).delete(id);
      tx.oncomplete = res; tx.onerror = res; tx.onabort = res;
    });
  } catch {}
}
// 报障诊断钩子：控制台执行 __tsDbg().then(console.log) 打印本地音频缓存各键尺寸
window.__tsDbg = async () => {
  const all = await _idbAll();
  return all.map(x => ({ k: String(x.id).slice(0, 30), size: x.size || (x.blob && x.blob.size) || 0 }));
};
async function idbPut(id, blob) {
  try {
    const db = await idbOpen();
    await new Promise(res => {
      const tx = db.transaction(AUDIO_STORE, 'readwrite');
      tx.objectStore(AUDIO_STORE).put({ id, blob, size: blob.size, lastUsed: Date.now() });
      tx.oncomplete = res; tx.onerror = res; tx.onabort = res;
    });
    // 超上限淘汰最久未用
    const all = await _idbAll();
    let total = all.reduce((s, x) => s + (x.size || 0), 0);
    if (total > AUDIO_MAX_BYTES) {
      all.sort((a, b) => (a.lastUsed || 0) - (b.lastUsed || 0));
      const tx = db.transaction(AUDIO_STORE, 'readwrite');
      const st = tx.objectStore(AUDIO_STORE);
      for (const x of all) { if (total <= AUDIO_MAX_BYTES) break; st.delete(x.id); total -= (x.size || 0); }
    }
  } catch {}
}
async function idbClear() {
  try {
    const db = await idbOpen();
    await new Promise(res => {
      const tx = db.transaction(AUDIO_STORE, 'readwrite');
      tx.objectStore(AUDIO_STORE).clear(); tx.oncomplete = res; tx.onerror = res;
    });
  } catch {}
}
async function idbStats() {
  try {
    const all = await _idbAll();
    return { count: all.length, bytes: all.reduce((s, x) => s + (x.size || 0), 0) };
  } catch { return { count: 0, bytes: 0 }; }
}
// 缓存优先取句子音频 blob URL
async function audioBlobURL(audioId) {
  const hit = await idbGet(audioId);
  if (hit && hit.blob && hit.blob.size >= 1024) { idbPut(audioId, hit.blob); return URL.createObjectURL(hit.blob); }
  const r = await fetchAuth('/api/v1/audio/' + audioId + '/file');
  if (!r.ok) throw new Error('音频获取失败 HTTP ' + r.status);
  const blob = await r.blob();
  idbPut(audioId, blob);
  return URL.createObjectURL(blob);
}
// 缓存优先取单词发音 blob URL（key 加 w2: 前缀与句子音频区分；w2 使旧词音缓存全部失效，
// 2026-11-07 修复「点/てん」被读成近似「せん」后需要重新拉取）
async function wordAudioBlobURL(wordId) {
  const key = 'w2:' + wordId;
  const hit = await idbGet(key);
  if (hit && hit.blob && hit.blob.size >= 1024) { idbPut(key, hit.blob); return URL.createObjectURL(hit.blob); }
  const r = await fetchAuth('/api/v1/word-study/' + wordId + '/audio');
  if (!r.ok) throw new Error('HTTP ' + r.status);
  const blob = await r.blob();
  idbPut(key, blob);
  return URL.createObjectURL(blob);
}

async function audioURL(s) {
  const a = (s.audios || [])[0];
  if (!a) return null;
  return audioBlobURL(a.id);
}

// 首页缓存卡片：显示占用 + 清理入口（FR-10.5 缓存管理）
async function updateCacheStats() {
  const el = $('cache-desc');
  if (!el) return;
  const st = await idbStats();
  const size = st.bytes >= 1024 * 1024
    ? (st.bytes / 1024 / 1024).toFixed(1) + ' MB'
    : Math.round(st.bytes / 1024) + ' KB';
  el.textContent = st.count
    ? `已缓存 ${st.count} 条音频 · ${size}（重复听不再下载）`
    : '尚未缓存音频，播放过的句子会自动存到本地。';
}
async function clearAudioCache() {
  if (!confirm('清理本地音频缓存？清理后下次播放会重新下载。')) return;
  await idbClear();
  updateCacheStats();
}

// playOne 播放单句并返回 Promise（连播顺序 await 用）。
// 未配音的句子点击时自动现场配音（单句 TTS 约 1—2 秒，即点即听）。
function playOne(s, idx, div) {
  return new Promise(async resolve => {
    const btn = div ? div.querySelector('.play') : null;
    try {
      stopPlay();
      document.querySelectorAll('.sent').forEach(x => x.classList.remove('now'));
      if (div) div.classList.add('now');
      if (!(s.audios || []).length) {
        if (btn) { btn.textContent = '⏳'; btn.classList.add('busy'); }
        await ensureAudio([s]);
        if (!(s.audios || []).length) { // 配音失败
          if (btn) { btn.textContent = '…'; btn.classList.remove('busy'); }
          if (div) div.classList.remove('now');
          return resolve();
        }
        // 局部更新按钮三态（不重建列表——译文/盲听/高亮等状态保持，修复重置 bug）
        if (btn) { btn.textContent = '▶'; btn.classList.add('has'); btn.classList.remove('busy'); }
        updateLearnHint();
      }
      const url = s._url || await audioURL(s);
      if (!url) { if (div) div.classList.remove('now'); return resolve(); }
      s._url = url; // 预签名 URL（10 分钟内有效，页面级缓存复用）
      playing = new Audio(url);
      const fin = () => { if (div) div.classList.remove('now'); playing = null; resolve(); };
      playing.onended = fin;
      playing.onerror = fin;
      await playing.play();
    } catch (e) {
      window._lastPlayErr = (e && e.name ? e.name + ': ' : '') + (e && e.message ? e.message : String(e));
      if (window._lastPlayErr.indexOf('NotAllowedError') >= 0) {
        // 非阻塞提示（alert 会挂起自动化测试/打断体验）：再点一次即可播放
        $('learn-hint').textContent = '⚠ 浏览器拦截了自动播放——请再点击一次句子即可播放';
      }
      if (div) div.classList.remove('now');
      playing = null; resolve();
    }
  });
}
function stopPlay() { if (playing) { playing.pause(); playing = null; } }

async function playSentence(s, idx, div) { await playOne(s, idx, div); }

async function playAll() {
  const btn = document.querySelector('#sc-learn .row2 .btn.ghost');
  let sents;
  if (VIEW === 'fav') {
    sents = (FAV_ITEMS || []).map(it => ({
      id: it.sentence_id, text: it.text,
      audios: it.audio_id ? [{ id: it.audio_id }] : [],
    }));
  } else if (VIEW === 'phrases') {
    sents = (phrasesMaterial() || {}).sentences || [];
  } else {
    sents = (LESSON.materials[CUR_MAT] || {}).sentences || [];
  }
  const need = sents.filter(s => !(s.audios || []).length).length;
  // 连播前自动补齐配音（不再无声跳过）
  if (need > 0) {
    const old = btn.textContent;
    btn.disabled = true;
    await ensureAudio(sents, (d, n) => (btn.textContent = `配音中 ${d}/${n}…`));
    btn.disabled = false; btn.textContent = old;
    if (VIEW !== 'fav') refreshSentenceButtons();
  }
  if (playing) stopPlay();
  for (let i = 0; i < sents.length; i++) {
    const div = document.querySelector(`.sent[data-idx="${i}"]`);
    await playOne(sents[i], i, div);
  }
}

// ============ 批量配音 ============
// ensureAudio 为传入句子中未配音者生成音频。
// 2026-10-27 架构决策（用户）：配音由服务端一步完成（synthesize），
// 前端仅缓存 audio_id 并经 302 播放；原 check→TTS→presign→PUT→register 链路废弃。
async function ensureAudio(sents, onProgress) {
  const voice = $('sel-voice').value;
  let done = 0, ok = 0, fail = 0;
  const total = sents.length;
  for (const s of sents) {
    if ((s.audios || []).length) { done++; ok++; continue; }
    try {
      const r = await api('/audio/synthesize', {
        method: 'POST',
        body: JSON.stringify({ sentence_id: s.id, voice, rate: 1.0 }),
      });
      s.audios = [{ id: r.audio_id }];
      s._url = null; // 新资产，预签名缓存失效
      ok++;
    } catch (e) {
      fail++;
      console.error(e);
      if (fail === 1) alert('有句子配音失败：' + e.message + '\n（后续失败仅在控制台记录，可重新点击重试）');
    }
    done++;
    if (onProgress) onProgress(done, total);
  }
  return { ok, fail };
}

async function genAllAudio() {
  const btn = $('tts-btn'); btn.disabled = true;
  const all = (LESSON.materials || []).flatMap(m => m.sentences);
  const r = await ensureAudio(all, (d, n) => (btn.textContent = `配音中 ${d}/${n}…`));
  btn.disabled = false;
  refreshSentenceButtons(); // 局部同步按钮与统计（不重置模式状态）
  alert(`配音完成：成功 ${r.ok} / 失败 ${r.fail}（失败句可重新点击补齐）`);
}

// ============ 复习 ============
let RV_ITEMS = [];   // 到期复习项
let RV_IDX = 0;      // 当前索引
let RV_PASS = 0;     // 本轮认识计数
let RV_FAIL = 0;     // 本轮不认识计数
let RV_AUDIO = null;  // 当前播放的 Audio

async function loadReviewCount() {
  const card = $('review-card');
  try {
    const r = await api('/reviews/due');
    const n = r.count || 0;
    if (n > 0) {
      card.style.display = 'block';
      $('review-desc').textContent = `有 ${n} 个句子需要复习`;
      RV_ITEMS = r.items || [];
    } else {
      card.style.display = 'none';
    }
  } catch {
    card.style.display = 'none';
  }
}

function goReview() {
  if (!RV_ITEMS.length) return;
  RV_IDX = 0; RV_PASS = 0; RV_FAIL = 0;
  go('review');
  renderReviewItem();
}

function renderReviewItem() {
  const box = $('review-content');
  if (RV_IDX >= RV_ITEMS.length) { renderReviewDone(); return; }
  const it = RV_ITEMS[RV_IDX];
  const s = it.sentence;
  const hasAudio = !!it.audio_id;
  const srcLabel = { marked: '难点', blind_low: '盲听', retell_stuck: '复述', manual: '手动' };
  box.innerHTML = `
    <div class="card">
      <div class="rv-progress">
        <div class="bar"><i style="width:${RV_ITEMS.length ? ((RV_IDX) / RV_ITEMS.length * 100) : 0}%"></i></div>
        <span>${RV_IDX + 1} / ${RV_ITEMS.length}</span>
      </div>
      <button class="rv-play" id="rv-play-btn" data-act="rv-play">${hasAudio ? '▶' : '…'}</button>
      <div id="rv-detail" style="display:none">
        <div class="rv-text">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
        <div class="rv-zh">${esc(s.translation || '')}</div>
        <div class="rv-meta">${srcLabel[it.source] || it.source} · 间隔 ${it.interval_days} 天 · 错 ${it.error_count} 次</div>
      </div>
      <div class="rv-actions" id="rv-reveal-row" style="display:none">
        <button class="btn ghost" data-act="rv-reveal">显示原文</button>
      </div>
      <div class="rv-actions" id="rv-result-row" style="display:none">
        <button class="btn" data-act="rv-fail">不认识</button>
        <button class="btn accent" data-act="rv-pass">认识</button>
      </div>
      <div style="text-align:center;margin-top:12px">
        <button class="hint" style="background:none;border:none;cursor:pointer;color:var(--ink2);font-size:12px"
          data-act="rv-master">已掌握，移出复习</button>
      </div>
    </div>
    <button class="btn ghost" data-act="home" style="margin-top:8px">返回首页</button>`;
  if (hasAudio) reviewPlayAudio();
  else $('rv-reveal-row').style.display = 'flex';
}

async function reviewPlayAudio() {
  const it = RV_ITEMS[RV_IDX];
  if (!it || !it.audio_id) return;
  const btn = $('rv-play-btn');
  try {
    if (RV_AUDIO) { RV_AUDIO.pause(); RV_AUDIO = null; }
    const url = await audioBlobURL(it.audio_id);
    RV_AUDIO = new Audio(url);
    if (btn) { btn.classList.add('playing'); btn.textContent = '⏳'; }
    RV_AUDIO.onended = () => {
      if (btn) { btn.classList.remove('playing'); btn.textContent = '▶'; }
      $('rv-reveal-row').style.display = 'flex';
    };
    RV_AUDIO.onerror = () => {
      if (btn) { btn.classList.remove('playing'); btn.textContent = '▶'; }
    };
    await RV_AUDIO.play();
  } catch (e) {
    if (btn) { btn.classList.remove('playing'); btn.textContent = '▶'; }
    $('rv-reveal-row').style.display = 'flex';
  }
}

function reviewReveal() {
  $('rv-detail').style.display = 'block';
  $('rv-reveal-row').style.display = 'none';
  $('rv-result-row').style.display = 'flex';
}

async function reviewResult(result) {
  const it = RV_ITEMS[RV_IDX];
  try {
    await api('/reviews/' + it.id + '/result', {
      method: 'POST',
      body: JSON.stringify({ result }),
    });
    if (result === 'pass') RV_PASS++; else RV_FAIL++;
  } catch (e) {
    alert(e.message);
  }
  RV_IDX++;
  renderReviewItem();
}

async function reviewMaster() {
  const it = RV_ITEMS[RV_IDX];
  if (!confirm('确定将此句标记为已掌握？它不会再出现在复习列表中。')) return;
  try {
    await api('/reviews/' + it.id + '/master', { method: 'POST' });
    RV_ITEMS.splice(RV_IDX, 1);
    if (RV_IDX >= RV_ITEMS.length && RV_ITEMS.length === 0) {
      renderReviewDone();
    } else {
      renderReviewItem();
    }
  } catch (e) {
    alert(e.message);
  }
}

function renderReviewDone() {
  const total = RV_PASS + RV_FAIL;
  $('review-content').innerHTML = `
    <div class="card rv-done-card">
      <h2>复习完成</h2>
      <div class="rv-stats">
        <div class="rv-stat pass"><div class="n">${RV_PASS}</div><div class="l">认识</div></div>
        <div class="rv-stat fail"><div class="n">${RV_FAIL}</div><div class="l">不认识</div></div>
        <div class="rv-stat"><div class="n">${total}</div><div class="l">总计</div></div>
      </div>
      <button class="btn accent" data-act="home">返回首页</button>
    </div>`;
}

// ============ 生词库 ============
// 跨课程累积的全部生词（word 表按用户持久）；点词复用用法索引，创建页可从这里选词。
let LIB_WORDS = [];
let LIB_Q = '';
let PICKED = new Set();
let PICK_Q = '';

async function loadLibWords() {
  const r = await api('/words/library');
  LIB_WORDS = r.items || [];
  return LIB_WORDS;
}

// loadLibCount 首页卡片：生词库数量
async function loadLibCount() {
  try {
    await loadLibWords();
    const d = $('lib-desc');
    if (!LIB_WORDS.length) d.textContent = '还没有生词。提交词单创建课程后，词语会自动累积到这里。';
    else d.textContent = `共 ${LIB_WORDS.length} 词 · 点词可看它的短语与句子，也可选词造新句。`;
  } catch {}
}

async function goLibrary() {
  stopPlay();
  try {
    if (!LIB_WORDS.length) await loadLibWords();
  } catch (e) { alert(e.message); return; }
  LIB_Q = ''; $('lib-search').value = '';
  renderLibrary();
  go('library');
}

function libSearch(v) { LIB_Q = v.trim(); renderLibrary(); }

function libFilter(list, q) {
  if (!q) return list;
  const s = q.toLowerCase();
  return list.filter(w =>
    (w.surface || '').toLowerCase().includes(s)
    || (w.reading || '').toLowerCase().includes(s)
    || (w.meaning || '').toLowerCase().includes(s));
}

function renderLibrary() {
  const box = $('lib-list'); box.innerHTML = '';
  const words = libFilter(LIB_WORDS, LIB_Q);
  if (!words.length) {
    box.innerHTML = `<p class="hint" style="text-align:center;margin-top:30px">${LIB_WORDS.length ? '没有匹配的词。' : '生词库是空的。提交词单创建课程后，词语会自动累积到这里。'}</p>`;
    $('lib-hint').textContent = '';
    return;
  }
  words.forEach(w => {
    const div = document.createElement('div');
    div.className = 'sent wordrow';
    div.innerHTML = `
      <span class="spk">${w.is_key ? '★' : ''}</span>
      <div class="wrap">
        <div class="txt"><ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby></div>
        <div class="zh">${esc(w.meaning || '')} ${esc(w.pos || '')} <span class="lib-meta">· ${w.lesson_count} 课${w.first_seen ? ' · 首见 ' + w.first_seen : ''}</span></div>
      </div>
      <button class="word-speak" title="听单词发音">🔊</button>
      <button class="play" title="展开用法">▾</button>`;
    div.querySelector('.word-speak').onclick = (e) => { e.stopPropagation(); wsSpeakWord(w.id, e.currentTarget); };
    div.onclick = () => toggleWordUsage(w, div);
    box.appendChild(div);
  });
  $('lib-hint').textContent = `生词库 · 共 ${LIB_WORDS.length} 词${LIB_Q ? ` · 匹配 ${words.length}` : ''} · 🔊 听词音 · 点词展开短语与句子`;
}

// openWordPicker 创建页「从生词库选词」：底部弹出选词层，确认后追加到词单输入框
async function openWordPicker() {
  try {
    if (!LIB_WORDS.length) await loadLibWords();
  } catch (e) { alert(e.message); return; }
  if (!LIB_WORDS.length) {
    alert('生词库是空的。先手动提交一次词单，之后的词都会自动累积进来。');
    return;
  }
  PICKED = new Set(); PICK_Q = '';
  $('picker-search').value = '';
  renderPickerList();
  $('picker-mask').style.display = 'flex';
}
function closePicker() { $('picker-mask').style.display = 'none'; }
function pickerSearch(v) { PICK_Q = v.trim(); renderPickerList(); }

function renderPickerList() {
  const box = $('picker-list'); box.innerHTML = '';
  const words = libFilter(LIB_WORDS, PICK_Q);
  if (!words.length) {
    box.innerHTML = '<p class="hint" style="text-align:center;margin-top:20px">没有匹配的词。</p>';
  }
  words.forEach(w => {
    const div = document.createElement('div');
    div.className = 'pick-row';
    div.innerHTML = `
      <input type="checkbox" ${PICKED.has(w.id) ? 'checked' : ''}>
      <div class="wrap">
        <div class="txt">${esc(w.surface)}${w.reading ? '（' + esc(w.reading) + '）' : ''}</div>
        <div class="zh">${esc(w.meaning || '')} ${esc(w.pos || '')}</div>
      </div>`;
    div.onclick = () => {
      if (PICKED.has(w.id)) PICKED.delete(w.id); else PICKED.add(w.id);
      div.querySelector('input').checked = PICKED.has(w.id);
      updatePickerOk();
    };
    box.appendChild(div);
  });
  updatePickerOk();
}

function updatePickerOk() {
  $('picker-ok').textContent = PICKED.size ? `加入词单（已选 ${PICKED.size} 词）` : '加入词单';
}

function pickerConfirm() {
  if (PICKED.size) {
    const lines = LIB_WORDS.filter(w => PICKED.has(w.id)).map(w =>
      [w.surface, w.reading || '', w.pos || '', w.meaning || ''].join('｜') + (w.is_key ? ' ★' : ''));
    const ta = $('words-input');
    const cur = ta.value.trim();
    ta.value = (cur ? cur + '\n' : '') + lines.join('\n');
    countWords();
  }
  closePicker();
}

// ============ 背词（词级 SRS）============
// 新学 = 无卡的最旧 20 词；复习 = 有卡未掌握且到期。翻卡：正面词面 →
// 翻面读音/词性/中文 + 例句（可点播）；点词面发单词音（/dev/tts 现场合成）。
let WS_MODE = 'new', WS_ITEMS = [], WS_IDX = 0, WS_PASS = 0, WS_FAIL = 0;
let WS_AUDIO = null; // 背词屏专用音频对象（与句子播放互不干扰）
const WS_VOICE = 'Nanami';

// loadWordStudyStatus 首页背词卡片：新学候选数 + 今日到期数
async function loadWordStudyStatus() {
  try {
    const st = await api('/word-study/status');
    const d = $('wordstudy-desc');
    const nb = $('ws-new-btn'), db = $('ws-due-btn');
    d.textContent = `新学 ${st.fresh} 词待学 · 今日 ${st.due} 词到期`;
    nb.disabled = !st.fresh;
    db.disabled = !st.due;
    db.textContent = st.due ? `复习（${st.due}）` : '复习 · 今日无到期';
    nb.textContent = st.fresh ? `新学（${Math.min(st.fresh, 20)}）` : '新学 · 已全部学过';
  } catch {}
}

// ---- 背词一级模式开关：标准/听力 两档（口语位预留、首版不显示），记住上次选择 ----
let WS_QUIZ_MODE = localStorage.getItem('ts_ws_mode') === 'listening' ? 'listening' : 'standard';
let WS_HINT_USED = false; // 本卡是否点开了语境提示（弱通过计分用）
let WS_GRADING = false;   // 作答提交中：挡住连点/回车+按钮双触发

function wsSetMode(m) {
  if (m === WS_QUIZ_MODE) return;
  WS_QUIZ_MODE = m;
  localStorage.setItem('ts_ws_mode', m);
  wsRestart(true); // 换通道=换题型池，进度与对错保留
}

// WS_SRC 会话来源：new/due 走记忆卡池，dir 走生词本目录整版
let WS_SRC = { kind: 'new' };
let WS_DONE = new Set(); // 本会话已计分的词 id：切模式不重复答

async function wsLoadItems() {
  const path = WS_SRC.kind === 'dir'
    ? '/word-study/dir/' + WS_SRC.id + '/quiz?mode=' + WS_QUIZ_MODE
    : '/word-study/' + WS_SRC.kind + '?mode=' + WS_QUIZ_MODE;
  const r = await api(path);
  return r.words || [];
}

async function wsRestart(keepProgress) {
  const from = keepProgress ? WS_IDX : 0;
  const pass = keepProgress ? WS_PASS : 0;
  const fail = keepProgress ? WS_FAIL : 0;
  if (!keepProgress) WS_DONE = new Set();
  try {
    WS_ITEMS = await wsLoadItems();
  } catch (e) { alert(e.message); return; }
  if (!WS_ITEMS.length) {
    alert(WS_SRC.kind === 'new' ? '没有待学的新词了。'
      : WS_SRC.kind === 'due' ? '今日没有到期的复习词。' : '这个目录没有可测的词。');
    return;
  }
  // 切模式只换题型不换进度：位置/对错保留，已答过的词跳过不重复计分
  WS_PASS = pass; WS_FAIL = fail;
  WS_IDX = wsNextUndone(Math.min(from, WS_ITEMS.length - 1));
  go('wordstudy');
  renderWordStudyItem();
}

// wsNextUndone 从 i 起找第一个本会话没答过的词；全答完返回 items.length
function wsNextUndone(i) {
  while (i < WS_ITEMS.length && WS_DONE.has(WS_ITEMS[i].id)) i++;
  return i;
}

// nbStartTest 生词本目录横条「测」：本目录全部词一次过完，成绩照进记忆卡排期。
// 用户要的就是"句子挖空测"，入口直接落到听力档（seg 仍可切回标准）。
async function nbStartTest() {
  if (!NB_CURDIR) { toast('先选择目录'); return; }
  WS_QUIZ_MODE = 'listening';
  localStorage.setItem('ts_ws_mode', WS_QUIZ_MODE);
  WS_MODE = 'dir';
  WS_SRC = { kind: 'dir', id: NB_CURDIR };
  await wsRestart(false);
}

function wsSegBar() {
  const b = (m, label) => `<button class="${WS_QUIZ_MODE === m ? 'on' : ''}" data-act="ws-seg" data-arg="${m}">${label}</button>`;
  return `<div class="ws-seg">${b('standard', '标准')}${b('listening', '听力')}</div>`;
}

async function goWordStudy(mode) {
  WS_MODE = mode;
  WS_SRC = { kind: mode };
  await wsRestart(false);
}

function wsStopAudio() {
  if (WS_AUDIO) { WS_AUDIO.pause(); WS_AUDIO = null; }
  clipStopAll();
}

// wsSpeakWord 单词发音：后端 /word-study/{id}/audio（首次合成落库，之后直接用缓存）
function wsSpeakWord(wordId, btn, retry) {
  return new Promise(async resolve => {
    try {
      if (btn) { btn.textContent = '⏳'; }
      const url = await wordAudioBlobURL(wordId);
      wsStopAudio();
      WS_AUDIO = new Audio(url);
      const a = WS_AUDIO;
      WS_AUDIO.onended = () => { if (btn) btn.textContent = '🔊'; resolve(); };
      WS_AUDIO.onerror = () => { if (btn) btn.textContent = '🔊'; toast('播放失败：音频解码错误'); resolve(); };
      await playUnlock(a);
      // 播了但不走字 = 本地缓存坏流：丢缓存重取一次（2026-11-06 用户设备曾缓存静音块）
      setTimeout(() => {
        if (!retry && WS_AUDIO === a && !a.paused && a.currentTime < 0.05) {
          wsStopAudio(); idbDel('w2:' + wordId); wsSpeakWord(wordId, btn, true);
        }
      }, 800);
      setTimeout(() => wsAudioDbg(a, '词'), 900);
    } catch (e) {
      if (btn) btn.textContent = '🔊';
      toast('词音失败：' + e.message);
      resolve();
    }
  });
}

// wsPlaySentence 例句点播：有音频直接播，没有先 synthesize 再播
async function wsPlaySentence(it, btn) {
  try {
    if (btn) { btn.textContent = '⏳'; }
    let audioId = it.audio_id;
    if (!audioId) {
      const r = await api('/audio/synthesize', {
        method: 'POST',
        body: JSON.stringify({ sentence_id: it.sentence_id, voice: WS_VOICE, rate: 1.0 }),
      });
      audioId = r.audio_id;
    }
    const url = await audioBlobURL(audioId);
    wsStopAudio();
    WS_AUDIO = new Audio(url);
    WS_AUDIO.onended = WS_AUDIO.onerror = () => { if (btn) btn.textContent = '▶'; };
    await WS_AUDIO.play();
  } catch (e) {
    if (btn) btn.textContent = '▶';
  }
}

// wsTokensHTML 挖空句逐 token 渲染：空白 token 不带振假名（注音即答案）；
// withInput 时空白直接是输入框（用户要求输入框长在空格位置上）
function wsTokensHTML(q, withInput) {
  return (q.tokens || []).map(t => t.blank
    ? (withInput
      ? '<input class="ws-inline-in" id="ws-type-in" maxlength="24" autocomplete="off" aria-label="输入空格里的词">'
      : '<span class="ws-blank">＿＿＿</span>')
    : (t.reading ? `<ruby>${esc(t.text)}<rt>${esc(t.reading)}</rt></ruby>` : esc(t.text))).join('');
}

// 揭晓面把空白填回并高亮目标词
function wsTokensFullHTML(q, w) {
  const fill = q.blank_text || w.surface;
  return (q.tokens || []).map(t => t.blank
    ? `<span class="ws-answord">${esc(fill)}</span>`
    : (t.reading ? `<ruby>${esc(t.text)}<rt>${esc(t.reading)}</rt></ruby>` : esc(t.text))).join('');
}

function wsOptsHTML(q) {
  return `<div class="ws-opts">${(q.options || []).map((o, i) =>
    `<button class="btn ghost ws-opt" data-i="${i}">${esc(o)}</button>`).join('')}</div>`;
}

// wsPlayQuizSent 挖空卡句子音：课程句走 audio_asset，生词本句走 /audio/say 文本缓存
async function wsPlayQuizSent(btn) {
  const w = WS_ITEMS[WS_IDX];
  const q = w && w.quiz;
  if (!q || !q.audio) return;
  try {
    if (btn) btn.textContent = '⏳';
    if (q.audio.kind === 'say') {
      await sayAudio(q.audio.text, btn);
      if (btn) btn.textContent = '🎧'; // sayAudio 结束会写回 ▶，句子按钮保持 🎧
      return;
    }
    wsStopAudio();
    const url = await audioBlobURL(q.audio.id);
    WS_AUDIO = new Audio(url);
    WS_AUDIO.onended = WS_AUDIO.onerror = () => { if (btn) btn.textContent = '🎧'; };
    await playUnlock(WS_AUDIO);
  } catch (e) {
    //  formerly 静默吞错：用户点 🎧 没声音也没提示，无法报障定位（2026-11-06）
    if (btn) btn.textContent = '🎧';
    toast('句子播放失败：' + e.message);
  }
}

function wsPrefetchWord(wordId) { wordAudioBlobURL(wordId).catch(() => {}); }

// wsPrefetchSent 渲染时只预热音频缓存不播放：无手势的自动播在安卓上易被静音卡死，
// 预热后用户点 🎧 走本地缓存，play() 落在点击手势内，声音才出得来
async function wsPrefetchSent() {
  const q = WS_ITEMS[WS_IDX] && WS_ITEMS[WS_IDX].quiz;
  if (!q || !q.audio) return;
  try {
    if (q.audio.kind === 'say') {
      const key = 'say:' + q.audio.text;
      const hit = await idbGet(key);
      if (hit && hit.blob) return;
      const r = await fetchAuth('/api/v1/audio/say', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text: q.audio.text }),
      });
      if (r.ok) idbPut(key, await r.blob());
    } else {
      await audioBlobURL(q.audio.id);
    }
  } catch (e) { /* 预热失败不碍事，点 🎧 时再取并会报错 */ }
}

function wsFrontQuiz(w) {
  const q = w.quiz;
  const play = `<button class="ws-mini ws-playbtn" data-act="ws-sent" title="播放句子">🎧</button>`;
  const hint = q.hint != null
    ? `<details class="ws-hint"><summary>显示中文语境提示</summary><p>${esc(q.hint)}</p></details>`
    : '';
  if (q.type === 'pick_mean') {
    return `<div class="ws-word-line"><span class="ws-word-txt">${esc(w.surface)}</span>` +
      `<button class="ws-mini" data-act="ws-speak" data-arg="${w.id}" title="听单词发音">🔊</button></div>
      <p class="hint">看词选义：它的中文意思是？</p>${wsOptsHTML(q)}`;
  }
  if (q.type === 'hear_mean') {
    return `<div class="ws-word-line"><button class="ws-mini" data-act="ws-speak" data-arg="${w.id}" title="听单词发音">🔊</button></div>
      <p class="hint">听词选义：听音选中文意思</p>${wsOptsHTML(q)}`;
  }
  if (q.type === 'cloze_pick') {
    return `<div class="ws-word-line">${play}</div>
      <div class="ws-cloze">${wsTokensHTML(q)}</div>${hint}
      <p class="hint">听句选词：空格里是哪个词？</p>${wsOptsHTML(q)}`;
  }
  return `<div class="ws-word-line">${play}</div>
    <div class="ws-cloze">${wsTokensHTML(q, true)}</div>${hint}
    <p class="hint">听句写词：在空格里直接输入（假名或汉字均可），回车或点确认</p>
    <div class="ws-type-confirm"><button class="btn ghost" data-act="ws-type">确认</button></div>`;
}

function wsFrontFlip(w) {
  return `<div class="ws-word-line">
      <span class="ws-word-txt">${esc(w.surface)}</span>
      <button class="ws-mini" data-act="ws-speak" data-arg="${w.id}" title="听单词发音">🔊</button>
    </div>
    <p class="hint">回想它的读音和意思，然后翻面</p>
    <button class="btn ghost" data-act="ws-flip">翻面</button>`;
}

function wsBackFlipHTML(w) {
  return `<div class="ws-word-line">
      <ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby>
      <button class="ws-mini" data-act="ws-speak" data-arg="${w.id}" title="听单词发音">🔊</button>
    </div>
    <div class="ws-meaning">${esc(w.meaning || '')} <span class="lib-meta">${esc(w.pos || '')}${w.is_key ? ' · ★重点' : ''}</span></div>
    ${w.interval_days ? `<div class="rv-meta">间隔 ${w.interval_days} 天 · 错 ${w.error_count} 次</div>` : '<div class="rv-meta">首次学习</div>'}
    <div id="ws-clips"></div>
    <div id="ws-usage"><p class="hint">加载例句…</p></div>
    <div id="ws-nb"></div>
    <div class="rv-actions">
      <button class="btn" data-act="ws-fail">不认识</button>
      <button class="btn accent" data-act="ws-pass">认识</button>
    </div>`;
}

// 揭晓面两层：词释义第一层、句子翻译第二层；提示使用留痕
function wsBackQuizHTML(w, ok, chosen) {
  const q = w.quiz;
  let mark = '';
  if (q.type === 'pick_mean' || q.type === 'cloze_pick') {
    mark = `<div class="ws-opts">${q.options.map((o, i) => {
      let cls = 'btn ghost ws-opt';
      if (i === q.answer) cls += ' right'; else if (i === chosen) cls += ' wrong';
      return `<button class="${cls}" disabled>${esc(o)}</button>`; }).join('')}</div>`;
  } else if (q.type === 'cloze_type') {
    mark = `<p class="ws-ans ${ok ? 'ok' : 'no'}">你的输入：${esc(chosen || '（空）')}　正解：${esc(q.blank_text || w.surface)}</p>`;
  }
  return `<div class="ws-mark ${ok ? 'ok' : 'no'}">${ok ? '答对了' : '答错了'}</div>
    <div class="ws-word-line">
      <ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby>
      <button class="ws-mini" data-act="ws-speak" data-arg="${w.id}" title="听单词发音">🔊</button>
      ${q.audio ? `<button class="ws-mini" data-act="ws-sent" title="听整句">🎧</button>` : ''}
    </div>
    <div class="ws-meaning">${esc(w.meaning || '')} <span class="lib-meta">${esc(w.pos || '')}${w.is_key ? ' · ★重点' : ''}</span></div>
    ${q.tokens ? `<div class="ws-cloze ws-cloze-full">${wsTokensFullHTML(q, w)}</div>` : ''}
    ${q.trans ? `<div class="ws-trans">句子翻译：${esc(q.trans)}</div>` : ''}
    <div id="ws-clips"></div>
    ${WS_HINT_USED ? '<div class="rv-meta">本次使用了语境提示（弱通过：间隔不增长）</div>' : ''}
    ${w.interval_days ? `<div class="rv-meta">间隔 ${w.interval_days} 天 · 错 ${w.error_count} 次</div>` : '<div class="rv-meta">首次学习</div>'}
    ${mark}
    <div class="rv-actions"><button class="btn accent" data-act="ws-next">下一词</button></div>`;
}

function renderWordStudyItem() {
  wsStopAudio();
  const box = $('wordstudy-content');
  if (WS_IDX >= WS_ITEMS.length) { renderWordStudyDone(); return; }
  WS_HINT_USED = false;
  const w = WS_ITEMS[WS_IDX];
  const total = WS_ITEMS.length;
  const q = w.quiz;
  const isQuiz = q && q.type !== 'flip';
  box.innerHTML = `
    ${wsSegBar()}
    <div class="card">
      <div class="rv-progress">
        <div class="bar"><i style="width:${(WS_IDX / total * 100)}%"></i></div>
        <span>${WS_IDX + 1} / ${total}</span>
      </div>
      <div class="ws-front" id="ws-front">${isQuiz ? wsFrontQuiz(w) : wsFrontFlip(w)}</div>
      <div class="ws-back" id="ws-back" style="display:none"></div>
      <div id="ws-dbg" class="hint" style="margin-top:6px"></div>
    </div>
    <button class="btn ghost" data-act="ws-quit" style="margin-top:8px">返回首页</button>`;
  if (!isQuiz) return;
  box.querySelectorAll('.ws-opt').forEach(b => { b.onclick = () => wsPick(+b.dataset.i); });
  const inp = $('ws-type-in');
  if (inp) inp.onkeydown = e => { if (e.key === 'Enter') wsTypeConfirm(); };
  // 听力题面不自动播（无手势易被安卓静音卡死）：只预热缓存，等用户点 🎧
  if (q.type === 'hear_mean') wsPrefetchWord(w.id);
  if (q.type === 'cloze_pick' || q.type === 'cloze_type') wsPrefetchSent();
}

// wsFlip 翻面（原翻面卡）：显示读音释义 + 拉取该词的例句/短语
async function wsFlip() {
  $('ws-front').style.display = 'none';
  const back = $('ws-back');
  back.style.display = 'block';
  back.innerHTML = wsBackFlipHTML(WS_ITEMS[WS_IDX]);
  const w = WS_ITEMS[WS_IDX];
  loadWordClips(w, $('ws-clips'), { title: '原声助记' });
  const box = $('ws-usage');
  try {
    const u = await api('/words/' + w.id + '/usage');
    const items = u.items || [];
    if (!items.length) { box.innerHTML = '<p class="hint">该词暂无课程关联句子。</p>'; }
    else {
      box.innerHTML = '<div class="usage-head">它的短语与句子（点播）</div>';
      items.slice(0, 4).forEach(x => {
        const row = document.createElement('div');
        row.className = 'sent usage-sent';
        row.innerHTML = `
          <span class="spk">${x.type === 'phrases' ? '短' : '句'}</span>
          <div class="wrap"><div class="txt">${rubyHTML(x.ruby_tokens) || esc(x.text)}</div>
          <div class="zh">${esc(x.translation || '')}</div></div>
          <button class="play ${x.audio_id ? 'has' : ''}">${x.audio_id ? '▶' : '…'}</button>`;
        row.querySelector('.play').onclick = (e) => { e.stopPropagation(); wsPlaySentence(x, e.target); };
        box.appendChild(row);
      });
    }
  } catch (e) {
    box.innerHTML = '<p class="hint">例句加载失败：' + esc(e.message) + '</p>';
  }
  // 生词本绑定例句（梁爽讲义等导入源）
  const nb = $('ws-nb');
  try {
    const r = await api('/word-study/' + w.id + '/sentences');
    const its = r.items || [];
    if (its.length) {
      nb.innerHTML = '<div class="usage-head">生词本例句</div>';
      its.slice(0, 4).forEach(x => {
        const row = document.createElement('div');
        row.className = 'sent usage-sent';
        row.innerHTML = `
          <span class="spk">${x.src === 'llm' ? '补' : '本'}</span>
          <div class="wrap"><div class="txt">${rubyHTML(x.ruby_tokens) || esc(x.text)}</div>
          <div class="zh">${esc(x.translation || '')}</div></div>`;
        nb.appendChild(row);
      });
    } else nb.innerHTML = '';
  } catch { nb.innerHTML = ''; }
}

// wsGrade 只提交不翻页；hint_used 让服务端走弱通过
async function wsGrade(result) {
  const w = WS_ITEMS[WS_IDX];
  await api('/word-study/' + w.id + '/result', {
    method: 'POST',
    body: JSON.stringify({ result, hint_used: WS_HINT_USED }),
  });
  WS_DONE.add(w.id);
  if (result === 'pass') WS_PASS++; else WS_FAIL++;
}

function wsShowReview(w, ok, chosen) {
  $('ws-front').style.display = 'none';
  const back = $('ws-back');
  back.style.display = 'block';
  back.innerHTML = wsBackQuizHTML(w, ok, chosen);
  loadWordClips(w, $('ws-clips'), { title: '原声助记' });
}

// wsAutoPlayFront 下一词落位后主动播一次题面音频：听力题播句子，其余播单词。
// 只在「下一词」点击手势里调用；首次渲染不自动播（无手势时安卓易被静音卡住）。
function wsAutoPlayFront() {
  if (WS_IDX >= WS_ITEMS.length) return;
  const it = WS_ITEMS[WS_IDX];
  const q = it.quiz;
  const front = $('ws-front');
  if (q && (q.type === 'cloze_pick' || q.type === 'cloze_type')) {
    if (q.audio) wsPlayQuizSent(front.querySelector('.ws-playbtn'));
    return;
  }
  wsSpeakWord(it.id, front.querySelector('[data-act="ws-speak"]'));
}

function wsNext() { WS_IDX = wsNextUndone(WS_IDX + 1); renderWordStudyItem(); wsAutoPlayFront(); }

async function wsPick(i) {
  if (WS_GRADING) return; // 连点防护：一次作答只提交一次
  WS_GRADING = true;
  const w = WS_ITEMS[WS_IDX];
  const ok = i === w.quiz.answer;
  document.querySelectorAll('#ws-front .ws-opt').forEach(b => { b.disabled = true; });
  try { await wsGrade(ok ? 'pass' : 'fail'); } catch (e) { WS_GRADING = false; alert(e.message); return; }
  wsShowReview(w, ok, i);
  WS_GRADING = false;
}

function wsNorm(s) { return (s || '').replace(/\s+/g, ''); }

async function wsTypeConfirm() {
  if (WS_GRADING) return;
  const inp = $('ws-type-in');
  if (!inp) return;
  WS_GRADING = true;
  const v = inp.value;
  const w = WS_ITEMS[WS_IDX];
  const acc = (w.quiz && w.quiz.accept && w.quiz.accept.length) ? w.quiz.accept : [w.surface, w.reading || ''];
  const ok = acc.some(x => wsNorm(v) === wsNorm(x));
  inp.disabled = true;
  try { await wsGrade(ok ? 'pass' : 'fail'); } catch (e) { WS_GRADING = false; alert(e.message); return; }
  wsShowReview(w, ok, v);
  WS_GRADING = false;
}

async function wsResult(result) {
  if (WS_GRADING) return;
  WS_GRADING = true;
  try { await wsGrade(result); } catch (e) { WS_GRADING = false; alert(e.message); return; }
  WS_IDX++;
  renderWordStudyItem();
  WS_GRADING = false;
}

function wsQuit() {
  wsStopAudio();
  loadWordStudyStatus();
  loadHome();
}

function renderWordStudyDone() {
  const total = WS_PASS + WS_FAIL;
  const label = WS_MODE === 'new' ? '新学完成' : WS_MODE === 'dir' ? '目录测试完成' : '复习完成';
  $('wordstudy-content').innerHTML = `
    <div class="card rv-done-card">
      <h2>${label}</h2>
      <div class="rv-stats">
        <div class="rv-stat pass"><div class="n">${WS_PASS}</div><div class="l">认识</div></div>
        <div class="rv-stat fail"><div class="n">${WS_FAIL}</div><div class="l">不认识</div></div>
        <div class="rv-stat"><div class="n">${total}</div><div class="l">总计</div></div>
      </div>
      <p class="hint" style="text-align:center;margin-top:6px">认识的词明天到期，间隔会逐次翻倍；不认识的明天就会再出现。</p>
      <button class="btn accent" data-act="ws-quit">返回首页</button>
    </div>`;
  loadWordStudyStatus();
}

// ============ 生词本浏览 ============
let NB_LIST = [], NB_CUR = null, NB_DIRS = [], NB_WORDS = {}, NB_CURDIR = null;

async function loadNbCount() {
  try {
    const r = await api('/notebooks');
    const items = r.items || [];
    const total = items.reduce((s, x) => s + x.word_count, 0);
    $('nb-desc').textContent = items.length
      ? `共 ${items.length} 本 · ${total} 词 · 按目录浏览导入的生词与例句。`
      : '还没有生词本。';
  } catch {}
}

async function goNotebook() {
  stopPlay();
  try { NB_LIST = (await api('/notebooks')).items || []; } catch (e) { alert(e.message); return; }
  if (!NB_LIST.length) { alert('还没有生词本。'); return; }
  const savedNb = localStorage.getItem('ts_nb_cur');
  if (!NB_CUR || !NB_LIST.find(x => x.id === NB_CUR)) NB_CUR = (savedNb && NB_LIST.find(x => x.id === savedNb)) ? savedNb : NB_LIST[0].id;
  await nbLoadTree();
  go('notebook');
  const savedDir = localStorage.getItem('ts_nb_dir');
  if (savedDir && NB_DIRS.find(d => d.id === savedDir)) await nbOpenDir(savedDir);
  else if (!NB_CURDIR) openNbPicker();
}

async function nbSwitchTo(id) {
  NB_CUR = id; NB_WORDS = {}; NB_CURDIR = null;
  localStorage.setItem('ts_nb_cur', id);
  localStorage.removeItem('ts_nb_dir');
  await nbLoadTree();
}

async function nbLoadTree() {
  try { NB_DIRS = (await api('/notebooks/' + NB_CUR + '/tree')).dirs || []; } catch (e) { alert(e.message); return; }
  renderNbStrip();
  $('nb-words').innerHTML = '';
  $('nb-hint').textContent = '点上方横条选择目录';
}

function nbChildren(pid) {
  return NB_DIRS.filter(d => (d.parent_id || '') === (pid || '')).sort((a, b) => a.sort_order - b.sort_order);
}

// 横条：展示关键信息（本名 · 目录路径 · 词数）
function nbDirPath(id) {
  const parts = [];
  let d = NB_DIRS.find(x => x.id === id);
  while (d) { parts.unshift(d.name); d = NB_DIRS.find(x => x.id === (d.parent_id || '')); }
  return parts.join('/');
}
function renderNbStrip() {
  const nb = NB_LIST.find(x => x.id === NB_CUR);
  const dir = NB_DIRS.find(x => x.id === NB_CURDIR);
  $('nb-strip-text').innerHTML = dir
    ? `<div class="nb-strip-row1"><b>${esc(nb ? nb.name : '生词本')}</b></div>` +
      `<div class="nb-strip-row2"><span class="nb-strip-path">${esc(nbDirPath(dir.id))}</span><span class="nb-strip-ct">${dir.word_count || 0} 词</span></div>`
    : `<div class="nb-strip-row1"><b>${esc(nb ? nb.name : '生词本')}</b></div>` +
      `<div class="nb-strip-row2"><span class="nb-strip-path">未选目录 · 点我选择</span></div>`;
  // 没选目录时复制/导出/测试没有对象，藏起来比点了报错好
  $('nb-copy-btn').hidden = !dir;
  $('nb-export-btn').hidden = !dir;
  $('nb-test-btn').hidden = !dir;
}

// 目录选择面板：横条正下方就地展开/收起（不再底部弹窗）
let NBP_OPEN = new Set();
function openNbPicker() {
  const sel = $('nbp-nb'); sel.innerHTML = '';
  NB_LIST.forEach(n => {
    const o = document.createElement('option');
    o.value = n.id; o.textContent = `${n.name}（${n.word_count} 词）`;
    sel.appendChild(o);
  });
  sel.value = NB_CUR;
  sel.onchange = async () => { await nbSwitchTo(sel.value); renderNbPickerList(); };
  NBP_OPEN = new Set(nbChildren('').map(d => d.id));
  if (NB_CURDIR) {
    let d = NB_DIRS.find(x => x.id === NB_CURDIR);
    while (d) { NBP_OPEN.add(d.id); d = NB_DIRS.find(x => x.id === (d.parent_id || '')); }
  }
  renderNbPickerList();
  $('nbp-panel').hidden = false;
  $('nbp-panel').scrollIntoView({ block: 'nearest' }); // 小屏上确保弹出来的面板在视口里
}
function closeNbPicker() { $('nbp-panel').hidden = true; }
// 点面板与横条之外收起
document.addEventListener('click', e => {
  const p = $('nbp-panel');
  if (!p || p.hidden) return;
  if (p.contains(e.target) || $('nb-strip').contains(e.target)) return;
  closeNbPicker();
});
function renderNbPickerList() {
  const box = $('nbp-list'); box.innerHTML = '';
  const walk = (pid, depth) => {
    nbChildren(pid).forEach(d => {
      const kids = nbChildren(d.id);
      const row = document.createElement('div');
      row.className = 'nb-dir' + (NB_CURDIR === d.id ? ' on' : '');
      row.style.paddingLeft = (12 + depth * 18) + 'px';
      row.innerHTML = `<span class="tw">${kids.length ? (NBP_OPEN.has(d.id) ? '▾' : '▸') : '·'}</span>` +
        `<span class="nm">${esc(d.name)}</span>` +
        `<button class="nb-dir-copy" title="${kids.length ? '复制整课的词和例句' : '复制本单元的词和例句'}">⧉</button>` +
        `<span class="ct">${d.word_count || ''}</span>`;
      // 复制按钮不能冒泡到行（否则变成展开/打开目录）
      row.querySelector('.nb-dir-copy').onclick = (e) => {
        e.stopPropagation();
        nbCopyDir(d.id, kids.length ? 'subtree' : 'self');
      };
      row.onclick = (e) => {
        e.stopPropagation(); // 重渲染后 target 脱离 DOM，否则会被外部点击收起逻辑误关
        if (kids.length) {
          NBP_OPEN.has(d.id) ? NBP_OPEN.delete(d.id) : NBP_OPEN.add(d.id);
          renderNbPickerList();
        } else {
          closeNbPicker();
          nbOpenDir(d.id);
        }
      };
      box.appendChild(row);
      if (kids.length && NBP_OPEN.has(d.id)) walk(d.id, depth + 1);
    });
  };
  walk('', 0);
}

async function nbOpenDir(dirId) {
  nbPlayStop();
  NB_CURDIR = dirId;
  localStorage.setItem('ts_nb_dir', dirId);
  renderNbStrip();
  const box = $('nb-words'); box.innerHTML = '<p class="hint">加载…</p>';
  let r;
  try { r = await api('/notebooks/dir/' + dirId + '/words'); }
  catch { box.innerHTML = '<p class="hint">加载失败</p>'; return; }
  NB_WORDS[dirId] = r.items || [];
  renderNbWords();
  // 句式条目（〜さん之类）后台补具体例句：fire-and-forget，补完后听力题才有意义可听
  api('/word-study/dir/' + dirId + '/enrich', { method: 'POST' }).catch(() => {});
}

function renderNbWords() {
  const items = NB_WORDS[NB_CURDIR] || [];
  const box = $('nb-words'); box.innerHTML = '';
  items.forEach((w, i) => {
    const div = document.createElement('div');
    div.className = 'sent wordrow';
    div.innerHTML = `
      <span class="spk">${i + 1}</span>
      <div class="wrap"><div class="txt">${w.ruby_tokens && w.ruby_tokens.length ? rubyHTML(w.ruby_tokens, 'plain') : `<ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby>`}</div>
      <div class="zh">${esc(w.meaning || '')} ${esc(w.pos || '')}</div></div>
      <button class="word-speak" title="听单词发音">🔊</button>
      <button class="play" title="展开例句">▾</button>`;
    div.querySelector('.word-speak').onclick = (e) => { e.stopPropagation(); wsSpeakWord(w.word_id, e.currentTarget); };
    div.onclick = () => { if (PL && nbPlayJumpTo(w)) return; nbToggleWord(w, div); };
    box.appendChild(div);
  });
  $('nb-hint').textContent = `共 ${items.length} 词 · 🔊 听词音 · 点词展开例句（▶ 点播）`;
}

// ---- 本单元导出/复制：数据已在 NB_WORDS 里（DirWords 接口返回词+例句），纯前端拼装 ----
// nbFormatWords 词+例句纯文本：多目录时按目录分组加小标题（整课复制用）
function nbFormatWords(title, items) {
  const L = [`${title}　共 ${items.length} 词`, ''];
  const dirs = [...new Set(items.map(w => w.dir).filter(Boolean))];
  const multi = dirs.length > 1;
  let cur = null;
  let n = 0;
  items.forEach((w) => {
    if (multi && w.dir !== cur) {
      cur = w.dir;
      L.push(`【${cur}】`, '');
    }
    n++;
    L.push(`${n}. ${w.surface}${w.reading ? '（' + w.reading + '）' : ''}${w.pos ? ' ' + w.pos : ''}${w.meaning ? ' ' + w.meaning : ''}`);
    (w.sentences || []).forEach(s => {
      L.push(`   ${s.text}`);
      if (s.translation) L.push(`   ${s.translation}`);
    });
    L.push('');
  });
  return L.join('\n');
}
function nbExportText() {
  const items = NB_WORDS[NB_CURDIR] || [];
  const dir = NB_DIRS.find(x => x.id === NB_CURDIR);
  return nbFormatWords(dir ? nbDirPath(dir.id) : '生词本', items);
}
// nbCopyText 写剪贴板：clipboard API 被拒时退 textarea+execCommand
async function nbCopyText(txt, n) {
  try {
    await navigator.clipboard.writeText(txt);
    toast(`已复制 ${n} 个词和全部例句`);
    return true;
  } catch (e) {
    const ta = document.createElement('textarea');
    ta.value = txt; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    toast(ok ? `已复制 ${n} 个词和全部例句` : '复制失败，请改用导出');
    return ok;
  }
}
// nbCopyDir 目录选择面板的复制按钮：课=整课子树，单元=仅本单元
async function nbCopyDir(dirId, scope) {
  try {
    const r = await api('/notebooks/dir/' + dirId + '/copy?scope=' + scope);
    await nbCopyText(nbFormatWords(r.title, r.items || []), (r.items || []).length);
  } catch (e) {
    toast('复制失败：' + e.message);
  }
}
function nbExportCsv() {
  const q = (v) => '"' + String(v == null ? '' : v).replace(/"/g, '""') + '"';
  const rows = [['单词', '假名', '词性', '释义', '例句', '例句翻译'].map(q).join(',')];
  (NB_WORDS[NB_CURDIR] || []).forEach(w => {
    const ss = w.sentences || [];
    rows.push([
      w.surface, w.reading || '', w.pos || '', w.meaning || '',
      ss.map(s => s.text).join(' / '), ss.map(s => s.translation || '').join(' / '),
    ].map(q).join(','));
  });
  return '\uFEFF' + rows.join('\r\n'); // BOM：Excel 打开中文不乱码
}
function nbFileBase() {
  const dir = NB_DIRS.find(x => x.id === NB_CURDIR);
  const name = (dir ? nbDirPath(dir.id) : '生词本').replace(/[\\/:*?"<>|\s]+/g, '-');
  return `${name}-${new Date().toISOString().slice(0, 10)}`;
}
function nbDownload(filename, mime, content) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([content], { type: mime }));
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 3000);
}
async function nbCopyAll() {
  if (!NB_CURDIR) { toast('先选择目录'); return; }
  await nbCopyText(nbExportText(), (NB_WORDS[NB_CURDIR] || []).length);
}
function nbExportAll() {
  if (!NB_CURDIR) { toast('先选择目录'); return; }
  const base = nbFileBase();
  nbDownload(base + '.txt', 'text/plain;charset=utf-8', nbExportText());
  nbDownload(base + '.csv', 'text/csv;charset=utf-8', nbExportCsv());
  toast('已导出 txt + csv 两个文件');
}

function nbBuildSents(w) {
  const wrap = document.createElement('div');
  wrap.className = 'nb-sents';
  if (!w.sentences.length) wrap.innerHTML = '<p class="hint" style="margin-left:26px">暂无例句</p>';
  w.sentences.forEach((s, si) => {
    const row = document.createElement('div');
    row.className = 'nb-sent';
    row.innerHTML = `<div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div><div class="zh">${esc(s.translation || '')}</div></div>`;
    row.onclick = (e) => {
      e.stopPropagation();
      if (PL) nbPlayJumpToSent(w, si);
    };
    const ask = document.createElement('button');
    ask.className = 'ai-ask'; ask.textContent = '问AI'; ask.title = '就这句问 AI';
    ask.onclick = (e) => { e.stopPropagation(); openAiAsk(s, w); };
    row.appendChild(ask);
    const btn = document.createElement('button');
    btn.className = 'play'; btn.textContent = '▶'; btn.title = '点播';
    btn.onclick = (e) => { e.stopPropagation(); sayAudio(s.text, btn); };
    row.appendChild(btn);
    wrap.appendChild(row);
  });
  return wrap;
}

function nbToggleWord(w, div) {
  const ex = div.querySelector('.nb-sents');
  if (ex) { ex.remove(); return; }
  document.querySelectorAll('.nb-sents').forEach(x => x.remove());
  div.appendChild(nbBuildSents(w));
}

// sayAudio 任意文本发音：/audio/say（服务端 text_audio 缓存）+ 本地 IndexedDB 二级缓存
async function sayAudio(text, btn, retry) {
  try {
    if (btn) btn.textContent = '⏳';
    const key = 'say:' + text;
    let url;
    const hit = await idbGet(key);
    // 小于 1KB 的缓存块视为坏缓存（曾出现空/截断 blob 被照播成"没声音"）：丢弃重取
    if (hit && hit.blob && hit.blob.size >= 1024) {
      url = URL.createObjectURL(hit.blob);
    } else {
      const r = await fetchAuth('/api/v1/audio/say', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const blob = await r.blob();
      idbPut(key, blob);
      url = URL.createObjectURL(blob);
    }
    wsStopAudio();
    WS_AUDIO = new Audio(url);
    const a = WS_AUDIO;
    WS_AUDIO.onended = () => { if (btn) btn.textContent = '▶'; };
    WS_AUDIO.onerror = () => { if (btn) btn.textContent = '▶'; toast('播放失败：音频解码错误'); };
    await playUnlock(a);
    // 播了但不走字 = 本地缓存坏流：丢缓存重取一次（2026-11-06 用户设备曾缓存静音块）
    setTimeout(() => {
      if (!retry && WS_AUDIO === a && !a.paused && a.currentTime < 0.05) {
        wsStopAudio(); idbDel(key); sayAudio(text, btn, true);
      }
    }, 800);
    setTimeout(() => wsAudioDbg(a, '句'), 900);
  } catch (e) {
    if (btn) btn.textContent = '▶';
    toast('点播失败：' + e.message);
  }
}

// ============ 问AI（句子上下文对话，MiniMax M3 最高思考）============
let AI_CTX = null;  // {text, ruby_tokens, translation, word}

async function openAiAsk(s, w) {
  AI_CTX = {
    text: s.text, ruby_tokens: s.ruby_tokens || [], translation: s.translation || '',
    word: { surface: w.surface, reading: w.reading || '', meaning: w.meaning || '' },
  };
  $('ai-ctx').innerHTML = `
    <button class="ai-clear" id="ai-clear" title="清空这句的聊天记录">清空记录</button>
    <div class="txt" style="font-size:15px">${rubyHTML(AI_CTX.ruby_tokens) || esc(AI_CTX.text)}</div>
    <div class="zh">${esc(AI_CTX.translation)}</div>
    <div class="zh lib-meta">生词：${esc(AI_CTX.word.surface)}${AI_CTX.word.reading ? '（' + esc(AI_CTX.word.reading) + '）' : ''} ${esc(AI_CTX.word.meaning)}</div>`;
  $('ai-msgs').innerHTML = '<p class="hint">载入聊天记录…</p>';
  $('ai-q').value = '';
  go('aiask');
  $('ai-clear').onclick = async () => {
    try {
      await api('/ai/chat?text=' + encodeURIComponent(AI_CTX.text), { method: 'DELETE' });
    } catch (e) { toast('清空失败：' + e.message); return; }
    $('ai-msgs').innerHTML = '<p class="hint">可以问这句的任何问题：语法、助词、活用、使用场景、近义词辨析…</p>';
    toast('已清空这句的聊天记录');
  };
  const HINT = '<p class="hint">可以问这句的任何问题：语法、助词、活用、使用场景、近义词辨析…</p>';
  try {
    const r = await api('/ai/chat?text=' + encodeURIComponent(AI_CTX.text));
    const items = r.items || [];
    $('ai-msgs').innerHTML = items.length ? '' : HINT;
    for (const m of items) {
      if (m.role === 'user') aiBubble('user', esc(m.content));
      else aiBubble('ai', '<div class="ai-reply">' + mdRender(m.content) + '</div>');
    }
  } catch {
    $('ai-msgs').innerHTML = HINT;
  }
  setTimeout(() => $('ai-q').focus(), 50);
}

function aiBubble(role, html) {
  const d = document.createElement('div');
  d.className = 'ai-bub ' + role;
  d.innerHTML = html;
  $('ai-msgs').appendChild(d);
  d.scrollIntoView({ block: 'end' });
  return d;
}

// mdRender 轻量 Markdown 渲染：先整体转义（防 XSS）再套标签；
// 支持 围栏代码/行内代码/表格/标题/分割线/引用/有序无序列表/加粗/斜体/链接/段落换行。
function mdRender(src) {
  const codes = [];
  let s = String(src).replace(/```([\s\S]*?)```/g, (m, c) => {
    codes.push(esc(c));
    return '\u0000' + (codes.length - 1) + '\u0000';
  });
  s = esc(s);
  // 表格：连续的 | 行
  s = s.replace(/(^|\n)((?:\|[^\n]*\|(?:\n|$))+)/g, (m, pre, block) => {
    const rows = block.trim().split('\n');
    const isSep = r => /^[\s|:-]+$/.test(r) && r.includes('-');
    const cells = r => r.replace(/^\|/, '').replace(/\|$/, '').split('|').map(c => c.trim());
    let html = '<table>', head = true;
    for (const r of rows) {
      if (isSep(r)) continue;
      const tag = head ? 'th' : 'td';
      head = false;
      html += '<tr>' + cells(r).map(c => '<' + tag + '>' + c + '</' + tag + '>').join('') + '</tr>';
    }
    return pre + html + '</table>';
  });
  // 标题 / 分割线 / 引用 / 列表
  s = s.replace(/^#### (.+)$/gm, '<h6>$1</h6>')
    .replace(/^### (.+)$/gm, '<h5>$1</h5>')
    .replace(/^## (.+)$/gm, '<h4>$1</h4>')
    .replace(/^# (.+)$/gm, '<h3>$1</h3>')
    .replace(/^---+$/gm, '<hr>')
    .replace(/(^|\n)((?:&gt; ?.*(?:\n|$))+)/g, (m, pre, block) =>
      pre + '<blockquote>' + block.trim().split('\n').map(l => l.replace(/^&gt; ?/, '').trimEnd()).join('<br>') + '</blockquote>')
    .replace(/(^|\n)((?:[ \t]*[-*][ \t].+(?:\n|$))+)/g, (m, pre, block) =>
      pre + '<ul>' + block.trim().split('\n').map(l => '<li>' + l.replace(/^[ \t]*[-*][ \t]/, '') + '</li>').join('') + '</ul>')
    .replace(/(^|\n)((?:[ \t]*\d+\.[ \t].+(?:\n|$))+)/g, (m, pre, block) =>
      pre + '<ol>' + block.trim().split('\n').map(l => '<li>' + l.replace(/^[ \t]*\d+\.[ \t]/, '') + '</li>').join('') + '</ol>');
  // 行内：代码 / 加粗 / 斜体 / 链接
  s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[^*<])\*([^*\n]+)\*/g, '$1<em>$2</em>')
    .replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  // 段落：空行分段，段内换行 <br>；块级与代码占位不包 <p>
  s = s.split(/\n{2,}/).map(blk => {
    blk = blk.trim();
    if (!blk || /^\u0000\d+\u0000$/.test(blk)) return blk;
    if (/^<(h\d|ul|ol|table|blockquote|hr|pre)/.test(blk)) return blk;
    return '<p>' + blk.replace(/\n/g, '<br>') + '</p>';
  }).join('');
  // 还原围栏代码（内容已转义）
  s = s.replace(/\u0000(\d+)\u0000/g, (m, i) => '<pre><code>' + codes[+i].trim() + '</code></pre>');
  return s;
}

async function aiSend() {
  const q = $('ai-q').value.trim();
  if (!q || !AI_CTX || $('ai-send').disabled) return;
  $('ai-q').value = '';
  aiBubble('user', esc(q));
  const bub = aiBubble('ai',
    '<div class="ai-thinking">思考中<span class="dots"></span></div>' +
    '<pre class="ai-thinklive" hidden></pre>' +
    '<div class="ai-reply"></div>');
  const thk = bub.querySelector('.ai-thinking');
  const live = bub.querySelector('.ai-thinklive');
  const rep = bub.querySelector('.ai-reply');
  $('ai-send').disabled = true;
  let reply = '', reasoning = '';
  try {
    const r = await fetch('/api/v1/ai/ask', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + TOKEN },
      body: JSON.stringify({ ...AI_CTX, question: q }),
    });
    if (!r.ok) {
      const j = await r.json().catch(() => null);
      throw new Error(j && j.error ? j.error.message : 'HTTP ' + r.status);
    }
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let ev = '', data = '';
        for (const line of block.split('\n')) {
          if (line.startsWith('event:')) ev = line.slice(6).trim();
          else if (line.startsWith('data:')) data += line.slice(5).trim();
        }
        if (!data) continue;
        if (ev === 'think') {
          reasoning += JSON.parse(data);
          live.hidden = false;
          live.textContent = reasoning.length > 20000 ? '…' + reasoning.slice(-20000) : reasoning;
          live.scrollTop = live.scrollHeight;
        } else if (ev === 'text') {
          reply += JSON.parse(data);
          if (!rep.dataset.on) { rep.dataset.on = '1'; thk.style.display = 'none'; live.hidden = true; }
          rep.innerHTML = mdRender(reply);
          bub.scrollIntoView({ block: 'end' });
        } else if (ev === 'done') {
          const j = JSON.parse(data);
          reply = j.reply || reply;
          reasoning = j.reasoning || reasoning;
        } else if (ev === 'error') {
          throw new Error(JSON.parse(data).message || 'AI 异常');
        }
      }
    }
    if (!reply) throw new Error('AI 返回空回复');
    thk.style.display = 'none';
    live.remove();
    if (reasoning) {
      const det = document.createElement('details');
      det.className = 'ai-reason';
      det.innerHTML = '<summary>思考过程</summary>';
      const pre = document.createElement('pre');
      pre.textContent = reasoning;
      det.appendChild(pre);
      bub.insertBefore(det, rep);
    }
    rep.innerHTML = mdRender(reply);
  } catch (e) {
    thk.style.display = 'none';
    live.remove();
    rep.innerHTML = '<span class="ai-err">请求失败：' + esc(e.message) + '</span>';
  } finally {
    $('ai-send').disabled = false;
    bub.scrollIntoView({ block: 'end' });
  }
}

// ============ 列表连播（▶ 全播）============
// 顺序：每词读 word 次 → 其例句逐句各读 sent 次；整表循环 loop 次。仅日语音频。
let PL_CFG = (() => {
  const def = { word: 2, sent: 1, loop: 1, wgap: 1, sgap: 1, wrate: 1, srate: 1, wmean: true, smean: true };
  try { return Object.assign(def, JSON.parse(localStorage.getItem('ts_nb_play_cfg') || '{}')); }
  catch { return def; }
})();
let PL = null;

const ZH_VOICE = 'zh-CN-XiaoxiaoNeural'; // 中文含义/翻译专用音色

// zhSpeechText 全播朗读中文含义前的清洗：词义里混着音调（⓪①②）、词性（[名、他Ⅲ]）、
// 日语读法/词源夹注，中文 TTS 不该念出来。显示仍用原文，只清洗送合成的文本。
function zhSpeechText(s) {
  let t = String(s || '');
  t = t.replace(/[\u24EA\u2460-\u2473]/g, ' ');                       // 音调 ⓪①②…
  t = t.replace(/\[[^\]]*\]|［[^］]*］/g, ' ');                        // 词性等方括号标记
  t = t.replace(/[（(][^）)]*[）)]/g,
    m => /[\u3041-\u3096\u30A1-\u30FA\u30FC A-Za-z]/.test(m) ? ' ' : m); // 含假名/拉丁的夹注
  t = t.replace(/〔[^〕]*〕/g, ' ');                                   // 语源括注
  t = t.replace(/[\u3041-\u3096\u30A1-\u30FA\u30FC]+/g, ' ');         // 残留假名
  t = t.replace(/[A-Za-z][A-Za-z .'’-]*/g, ' ');                      // 英文/罗马字
  t = t.replace(/[·・｜|]/g, ' ').replace(/\s+/g, ' ').trim();
  t = t.replace(/\s+([，。；：、！？,;:.!?])/g, '$1');
  return t.replace(/^[,，、;；:：.。\-—]+|[,，、;；:：.。\-—]+$/g, '').trim();
}

async function sayBlobURL(text, voice) {
  const key = 'say:' + (voice || '') + ':' + text;
  const hit = await idbGet(key);
  if (hit && hit.blob && hit.blob.size >= 1024) { idbPut(key, hit.blob); return URL.createObjectURL(hit.blob); }
  const r = await fetchAuth('/api/v1/audio/say', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(voice ? { text, voice } : { text }),
  });
  if (!r.ok) throw new Error('HTTP ' + r.status);
  const blob = await r.blob();
  idbPut(key, blob);
  return URL.createObjectURL(blob);
}

function nbPlayItems() {
  const items = [];
  (NB_WORDS[NB_CURDIR] || []).forEach(w => {
    for (let k = 0; k < PL_CFG.word; k++) {
      items.push({ kind: 'word', w });
      const say = zhSpeechText(w.meaning);
      if (PL_CFG.wmean && say) items.push({ kind: 'wmean', w, say });
    }
    (w.sentences || []).forEach((s, si) => {
      for (let k = 0; k < PL_CFG.sent; k++) {
        items.push({ kind: 'sent', w, s, si });
        if (PL_CFG.smean && s.translation) items.push({ kind: 'smean', w, s, si });
      }
    });
  });
  return items;
}

async function nbPlayStart() {
  nbPlayStop();
  closeNbPicker();
  const items = nbPlayItems();
  if (!items.length) { toast('本目录没有可播放内容'); return; }
  PL = { items, i: 0, loop: 0, audio: new Audio(), follow: true, autoScrolling: false, paused: false };
  PL.audio.addEventListener('ended', () => nbPlayNext());
  PL.audio.addEventListener('error', () => { toast('音频播放出错，跳下一条'); nbPlayNext(); });
  // 部分移动端浏览器要等元数据就绪才认 playbackRate：加载后补设一次
  PL.audio.addEventListener('loadedmetadata', () => {
    if (!PL) return;
    const rate = rateFor(PL.items[PL.i]);
    PL.audio.defaultPlaybackRate = rate;
    PL.audio.playbackRate = rate;
  });
  $('nb-now-pause').textContent = '⏸ 暂停';
  $('nb-nowbar').hidden = false;
  document.body.classList.add('nb-playing');
  await nbPlayStep();
}

function nbPlayStop() {
  if (PL) { clearTimeout(PL.gapTimer); try { PL.audio.pause(); } catch (e) {} PL = null; }
  document.querySelectorAll('#nb-words .playing').forEach(x => x.classList.remove('playing'));
  const bar = $('nb-nowbar');
  if (bar) bar.hidden = true;
  document.body.classList.remove('nb-playing');
}

async function nbPlayNext() {
  if (!PL) return;
  const gap = gapFor(PL.items[PL.i]);
  PL.i++;
  if (PL.i >= PL.items.length) {
    PL.loop++;
    if (PL.loop >= PL_CFG.loop) { nbPlayStop(); toast('播放完成'); return; }
    PL.i = 0;
  }
  // 停顿用定时器实现；暂停时挂起，继续时立刻补上推进
  clearTimeout(PL.gapTimer);
  if (gap > 0) {
    PL.gapTimer = setTimeout(() => {
      if (!PL) return;
      if (PL.paused) { PL.gapPending = true; return; }
      nbPlayStep();
    }, gap * 1000);
  } else {
    await nbPlayStep();
  }
}

// 程序滚动窗口按"静默期"续期而非定时长：smooth 滚动实测可达 ~1s，
// 固定窗口会把动画尾部的 scroll 事件误判成用户滚动，导致跟随一次就永久失效。
let autoScrollTimer = 0, autoScrollDeadline = 0;
function beginAutoScroll() {
  PL.autoScrolling = true;
  autoScrollDeadline = Date.now() + 2500;
  armAutoScrollEnd();
}
function armAutoScrollEnd() {
  clearTimeout(autoScrollTimer);
  autoScrollTimer = setTimeout(() => { if (PL) PL.autoScrolling = false; }, 200);
}
// 用户明确手势：立刻结束程序滚动窗口并停止跟随
function userScrolled() {
  if (!PL) return;
  clearTimeout(autoScrollTimer);
  PL.autoScrolling = false;
  PL.follow = false;
}
// 跟随时把正在播放的词/句平滑滚到屏幕中央。
// 手动滚动接管后（follow=false）只要播放项还在视口里就不打扰；
// 一旦播放项跑出视口（接管所致或列表推进），立即拉回中央并恢复跟随——
// 手机上轻微划动就会触发接管，旧逻辑会让后半程彻底失去居中（2026-11-06 报障）。
function nbScrollToPlaying(el) {
  if (!PL) return;
  const r = el.getBoundingClientRect();
  const inView = r.top >= 0 && r.bottom <= innerHeight;
  if (!PL.follow && inView) return;
  beginAutoScroll();
  el.scrollIntoView({ block: 'center', behavior: 'smooth' });
  PL.follow = true;
}

// 播放中点词跳转：从该词第一条音频开始继续往后播
function nbPlayJumpTo(w) {
  if (!PL) return false;
  const idx = PL.items.findIndex(it => it.w === w);
  if (idx < 0) return false;
  nbPlayJumpToIndex(idx);
  return true;
}

// 播放中点句跳转：从该句音频开始继续往后播
function nbPlayJumpToSent(w, si) {
  if (!PL) return false;
  const idx = PL.items.findIndex(it => it.kind === 'sent' && it.w === w && it.si === si);
  if (idx < 0) return false;
  nbPlayJumpToIndex(idx);
  return true;
}

function nbPlayJumpToIndex(idx) {
  PL.i = idx;
  PL.follow = true;
  PL.paused = false;
  PL.gapPending = false;
  clearTimeout(PL.gapTimer);
  $('nb-now-pause').textContent = '⏸ 暂停';
  try { PL.audio.pause(); } catch (e) {}
  nbPlayStep();
}

// 方向键跳转：只在「单词读音 / 例句原声」之间走，跳过中文含义/句译与同一项的重复遍数
function nbPlayUnits() {
  const units = [];
  PL.items.forEach((it, i) => {
    if (it.kind !== 'word' && it.kind !== 'sent') return;
    const p = units[units.length - 1];
    if (p && p.kind === it.kind && p.w === it.w && p.si === it.si) return;
    units.push({ kind: it.kind, w: it.w, si: it.si, i });
  });
  return units;
}

function nbPlaySkip(step) {
  if (!PL) return false;
  const units = nbPlayUnits();
  let u = -1;
  for (let k = 0; k < units.length; k++) if (units[k].i <= PL.i) u = k;
  const t = units[u + step];
  if (!t) return false;
  nbPlayJumpToIndex(t.i);
  return true;
}

// 暂停/继续：同一 Audio 元素 pause/play，续播即从暂停处继续
function nbPlayPause() {
  if (!PL) return;
  if (PL.paused) {
    PL.paused = false;
    $('nb-now-pause').textContent = '⏸ 暂停';
    if (PL.gapPending) { PL.gapPending = false; nbPlayStep(); return; } // 停顿中暂停：继续时直接推进
    playUnlock(PL.audio).catch(() => nbPlayNext());
  } else {
    PL.paused = true;
    $('nb-now-pause').textContent = '▶ 继续';
    try { PL.audio.pause(); } catch (e) {}
  }
}

function nbPlayHighlight(it) {
  document.querySelectorAll('#nb-words .playing').forEach(x => x.classList.remove('playing'));
  const words = NB_WORDS[NB_CURDIR] || [];
  const row = document.querySelectorAll('#nb-words .wordrow')[words.indexOf(it.w)];
  if (!row) return;
  row.classList.add('playing');
  // 播放期只展开不折叠：向下增长不拉动视口，折叠上方内容才会造成跳动
  if (!row.querySelector('.nb-sents')) row.appendChild(nbBuildSents(it.w));
  const eff = it.kind === 'wmean' ? 'word' : it.kind === 'smean' ? 'sent' : it.kind;
  if (eff === 'sent') {
    const sr = row.querySelectorAll('.nb-sent')[it.si];
    if (sr) { sr.classList.add('playing'); nbScrollToPlaying(sr); }
  } else {
    nbScrollToPlaying(row);
  }
  const wi = words.indexOf(it.w) + 1;
  const zh = it.kind === 'wmean' ? ` · 中文：${(it.say || '').slice(0, 14)}`
    : it.kind === 'smean' ? ` · 中文：${(it.s.translation || '').slice(0, 14)}` : '';
  const sent = eff === 'sent' ? ` · 句 ${it.si + 1}/${(it.w.sentences || []).length}：${it.s.text.slice(0, 18)}` : ' · 单词';
  $('nb-now-text').textContent = `遍 ${PL.loop + 1}/${PL_CFG.loop} · 词 ${wi}/${words.length} ${it.w.surface}${sent}${zh}`;
}

async function nbPlayStep(autoplay = true) {
  if (!PL) return;
  const it = PL.items[PL.i];
  nbPlayHighlight(it);
  const pl = PL, idx = PL.i; // 连续跳转时丢弃过期请求，避免旧音频盖掉新选的项
  try {
    let url;
    if (it.kind === 'word') url = await wordAudioBlobURL(it.w.word_id);
    else if (it.kind === 'wmean') url = await sayBlobURL(it.say, ZH_VOICE);
    else if (it.kind === 'smean') url = await sayBlobURL(it.s.translation, ZH_VOICE);
    else url = await sayBlobURL(it.s.text);
    if (PL !== pl || PL.i !== idx) return;
    PL.audio.src = url;
    const rate = rateFor(it);
    PL.audio.defaultPlaybackRate = rate;
    PL.audio.playbackRate = rate;
    if (autoplay) await playUnlock(PL.audio);
  } catch (e) {
    if (PL !== pl || PL.i !== idx) return;
    toast('获取音频失败，跳过：' + e.message);
    nbPlayNext();
  }
}

// gapFor 每条音频播完后的停顿秒数：词系/句系分开配
function gapFor(it) {
  if (!it) return 0;
  return (it.kind === 'word' || it.kind === 'wmean') ? (PL_CFG.wgap || 0) : (PL_CFG.sgap || 0);
}

// rateFor 每组音频的倍速：单词与其中文含义同速，例句与句译同速
function rateFor(it) {
  if (!it) return 1;
  return (it.kind === 'word' || it.kind === 'wmean') ? (PL_CFG.wrate || 1) : (PL_CFG.srate || 1);
}

// 预设里没有的旧倍速值（手工改过 localStorage 等）补一个选项，避免下拉显示空白
function setRateSel(id, v) {
  const sel = $(id);
  sel.value = String(v);
  if (sel.value) return;
  const opt = document.createElement('option');
  opt.value = String(v);
  opt.textContent = v + '×';
  sel.appendChild(opt);
  sel.value = String(v);
}

function openNbCfg() {
  $('cfg-word').value = PL_CFG.word;
  $('cfg-sent').value = PL_CFG.sent;
  $('cfg-loop').value = PL_CFG.loop;
  $('cfg-wgap').value = PL_CFG.wgap == null ? 1 : PL_CFG.wgap;
  $('cfg-sgap').value = PL_CFG.sgap == null ? 1 : PL_CFG.sgap;
  setRateSel('cfg-wrate', PL_CFG.wrate || 1);
  setRateSel('cfg-srate', PL_CFG.srate || 1);
  $('cfg-wmean').checked = !!PL_CFG.wmean;
  $('cfg-smean').checked = !!PL_CFG.smean;
  $('nb-cfg-mask').style.display = 'flex';
}
function saveNbCfg() {
  const c = v => Math.min(99, Math.max(1, parseInt(v, 10) || 1));
  const g = v => Math.min(10, Math.max(0, parseInt(v, 10) || 0));
  const rt = v => Math.min(2, Math.max(0.5, parseFloat(v) || 1));
  const prev = PL_CFG;
  PL_CFG = {
    word: c($('cfg-word').value), sent: c($('cfg-sent').value), loop: c($('cfg-loop').value),
    wgap: g($('cfg-wgap').value), sgap: g($('cfg-sgap').value),
    wrate: rt($('cfg-wrate').value), srate: rt($('cfg-srate').value),
    wmean: $('cfg-wmean').checked, smean: $('cfg-smean').checked,
  };
  localStorage.setItem('ts_nb_play_cfg', JSON.stringify(PL_CFG));
  $('nb-cfg-mask').style.display = 'none';
  toast(`已保存：词 ×${PL_CFG.word}·${PL_CFG.wrate}× · 句 ×${PL_CFG.sent}·${PL_CFG.srate}× · 循环 ×${PL_CFG.loop}`);
  if (PL) { // 倍速即时生效：当前这条音频不用重播就能变速
    PL.audio.defaultPlaybackRate = rateFor(PL.items[PL.i]);
    PL.audio.playbackRate = rateFor(PL.items[PL.i]);
  }
  const changed = prev.word !== PL_CFG.word || prev.sent !== PL_CFG.sent ||
    !!prev.wmean !== !!PL_CFG.wmean || !!prev.smean !== !!PL_CFG.smean;
  if (changed) nbPlayApplyCfg();
  else if (PL) nbPlayHighlight(PL.items[PL.i]); // 只改进度类设置时刷新「遍 x/y」显示
}

// 全播中改播放设置：按新配置重建序列，从当前所在的词/句块重新开始（间隔与循环次数本就实时生效）
function nbPlayApplyCfg() {
  if (!PL) return;
  const cur = PL.items[PL.i];
  const items = nbPlayItems();
  if (!items.length) { nbPlayStop(); return; }
  PL.items = items;
  let idx = -1;
  if (cur) {
    if (cur.kind === 'word' || cur.kind === 'wmean')
      idx = items.findIndex(x => x.kind === 'word' && x.w === cur.w);
    else
      idx = items.findIndex(x => x.kind === 'sent' && x.w === cur.w && x.si === cur.si);
  }
  if (idx < 0) idx = Math.min(PL.i, items.length - 1);
  clearTimeout(PL.gapTimer);
  PL.gapPending = false;
  PL.i = idx;
  PL.follow = true;
  try { PL.audio.pause(); } catch (e) {}
  nbPlayStep(!PL.paused);
}

// ============ 课文本（练口语课文：听读/背诵/听写/连听）============
// 参照 DashPlayer：整课一条原声音频 + SRT 行时间轴。当前句只由时间轴决定，
// 点句 seek、句末自动停（听读），连续播放自动高亮（连听）。
let REC_LIST = [];
let REC = null;            // {lesson, lines[]}
let REC_IDX = 0;
let REC_MODE = 'read';
let REC_REVEAL = false;
let REC_RATE = parseFloat(localStorage.getItem('ts_rec_rate') || '1') || 1;
let REC_AUDIO = null, REC_AUDIO_LESSON = null, REC_AUDIO_URL = '';
let REC_AUDIO_P = null, REC_AUDIO_P_ID = null;
let REC_STOP_AT = null;    // 秒；null = 连续播放
let REC_FOLLOW = true;
let REC_RAF = 0;
let REC_DICT_TRIES = 0;
let REC_DICT_LOCK = false;
let REC_DICT_CHECKING = false;
let REC_DICT_TIMER = 0;
let REC_ROLE = '';          // 当前扮演的角色（按课文记住）
let REC_ROLE_ACTIVE = false;
let REC_ROLE_FINISHED = false;
let REC_ROLE_PICKING = false;
let REC_ROLE_TIMER = 0;
let REC_ROLE_REVEAL = false;
let REC_ROLE_DONE = 0;
let REC_ROLE_TOTAL = 0;
let REC_ROLE_HINTS = 0;
let REC_ROLE_HINTED = new Set();
// 消失术
let REC_CLOZE_STEP = 0;
let REC_CLOZE_HIDDEN = new Set();
let REC_CLOZE_PEEK = new Set();
let REC_CLOZE_WORDS = [];
let REC_CLOZE_STARS = 3;
let REC_CLOZE_TOTAL = 0;
let REC_CLOZE_PERFECT = 0;
// 中译日
let REC_ZH_LVL = 0;
let REC_ZH_USED_THIS = false;
let REC_ZH_CLEAN = 0;
let REC_ZH_USED = 0;
// 通关赛
let REC_RUN_COMBO = 0;
let REC_RUN_BEST = 0;
let REC_RUN_PREV_BEST = 0;
let REC_RUN_CLEAN = 0;
let REC_RUN_USED = 0;
let REC_RUN_FLAGGED = [];
let REC_RUN_OT = [];
let REC_RUN_OT_POS = 0;
let REC_RUN_OT_HINTS = 0;
let REC_RUN_PHASE = 'main';   // main | overtime | done
let REC_RUN_REVEAL = false;   // 当前句正在展示答案
let REC_RUN_SEEN = false;     // 当前句看过答案（收起来也算）

const REC_DESC = {
  read: '点任意一句从头播放，句末自动停；点 ↻ 重听本句。',
  recite: '先听一句，再凭首字提示回忆整句。想不起来可看答案，背完点「会背了」。',
  dict: '听整句，输入日语原文后按回车检查。汉字写对或假名读音写对都算通过。',
  listen: '整段连续播放，当前句自动高亮。手动滚动页面会暂停跟随，点播放恢复。',
  role: '选一个角色：对方台词自动播放；轮到你时只给首字提示，说完点「接上了」。双方合说的句子自动播放。',
  cloze: '先看全文背一遍；每背对一次就多遮住一些词，最后只剩首字假名。点遮住的词可以偷看，但会掉一颗星。',
  zh2jp: '只看中文，先出声说出日语；卡住时按提示逐级加码：首字 → 每词首字 → 关键词 → 全句。',
  run: '一课一局：不看答案背完一句就加一次连击；看过答案的句子会在结尾自动进入最多 3 句加时赛。',
};

function recFmt(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}
function recTitle(x) {
  const t = x.title || '';
  return /^第\s*\d+\s*[课課]/.test(t) ? t : `第${x.lesson_no}課 · ${t}`;
}
function recLine() { return REC && REC.lines[REC_IDX]; }
function recTokens(l) {
  return Array.isArray(l.ruby_tokens)
    ? l.ruby_tokens.filter(t => Array.isArray(t) && t.length >= 2)
    : [];
}
function recIsPunct(s) {
  return /^[\s\u3000、。，．！？!?「」『』（）()【】〈〉《》…‥:：;；~〜\-—–・]+$/.test(s);
}
// 可挖空/可提示的“词”：取 ruby_tokens 里非标点的项，返回 token 下标。
function recWords(l) {
  const out = [];
  recTokens(l).forEach((t, i) => { if (t[0] && !recIsPunct(t[0])) out.push(i); });
  return out;
}
function recFirstKana(t) {
  const read = t[1] || '';
  const src = /[ぁ-ゖァ-ヺ]/.test(read) ? read : t[0];
  return Array.from(src || '')[0] || '';
}
function recFirstChar(t) {
  return Array.from((t && t[0]) || '')[0] || '';
}
function recJP(l) {
  const t = recTokens(l);
  return t.length ? rubyHTML(t) : esc(l.text);
}
function recMark(l) {
  if (l.dict_ok) return '听写 ✓';
  if (l.dict_reading) return '读音 ✓';
  return '';
}
function recSavePos() {
  if (REC) localStorage.setItem('ts_rec_pos_' + REC.lesson.id, String(REC.lines[REC_IDX].line_no));
}

// recNorm 听写比对用：去空白/标点、片假名转平假名、全角转半角；
// 保留长音ー、促音っ与浊音，避免把「びょういん」和「びよういん」判成一样。
function recNorm(s) {
  return String(s || '')
    .normalize('NFKC')
    .replace(/[\s\u3000]+/g, '')
    .replace(/[、。，．！？!?,.・「」『』（）()\[\]【】〈〉《》…‥:：;；~〜\-—–]/g, '')
    .replace(/[\u30A1-\u30F6]/g, c => String.fromCharCode(c.charCodeAt(0) - 0x60))
    .toLowerCase();
}
function recRe(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'); }

// 课文用假名书写、但常用汉字写法也应判对的词（读音相同）。
const REC_ALT_WORDS = [
  ['かに座', '蟹座'],
  ['いろいろ', '色々'],
];
// recAltTexts 返回原文及等价写法，供判对与差异对照使用。
function recAltTexts(text) {
  let out = [text];
  for (const [base, alt] of REC_ALT_WORDS) {
    if (!out.some(t => t.includes(base))) continue;
    out = [...new Set(out.flatMap(t => t.includes(base) ? [t, t.split(base).join(alt)] : [t]))];
  }
  return out;
}
// 等价写法的对照词表：被替换的整段并成一个 token，保留原有读音变体。
function recAltTokens(l, altText) {
  const toks = recTokens(l).map(t => [t[0], t[1]]);
  let text = l.text;
  for (const [base, alt] of REC_ALT_WORDS) {
    const at = text.indexOf(base);
    if (at < 0 || !altText.includes(alt)) continue;
    let pos = 0, i0 = -1, i1 = -1;
    for (let i = 0; i < toks.length; i++) {
      const n = String(toks[i][0] || '').length;
      if (pos + n > at && pos < at + base.length) {
        if (i0 < 0) i0 = i;
        i1 = i;
      }
      pos += n;
    }
    if (i0 < 0) continue;
    let head = 0, span = 0;
    for (let i = 0; i < i0; i++) head += String(toks[i][0] || '').length;
    for (let i = i0; i <= i1; i++) span += String(toks[i][0] || '').length;
    if (head !== at || head + span !== at + base.length) continue;
    const rd = toks.slice(i0, i1 + 1).map(t => t[1] || t[0]).join('');
    toks.splice(i0, i1 - i0 + 1, [alt, rd === alt ? null : rd]);
    text = text.slice(0, at) + alt + text.slice(at + base.length);
  }
  return text === altText ? toks : [];
}
// 词库汉字词按读音折叠：用户写「蟹座」而课文写「かに座」时按读音判对。
let REC_FOLD = { n: -1, pairs: [] };
function recFoldMap() {
  if (REC_FOLD.n === LIB_WORDS.length) return REC_FOLD.pairs;
  const pairs = [];
  for (const w of LIB_WORDS) {
    if (!w || !w.surface || !w.reading) continue;
    if (!/[\u4e00-\u9fff々]/.test(w.surface)) continue;
    // 只收词条，避免把词库里的整句和带占位符的短语折出错误读音
    if (Array.from(w.surface).length > 4 || /[\s、。〜]/.test(w.surface)) continue;
    const s = recNorm(w.surface), r = recNorm(w.reading);
    if (!s || !r || s === r) continue;
    if (/[^\u3041-\u3096\u30fc]/.test(r)) continue;
    pairs.push([s, r]);
  }
  // 课文假名 ↔ 常用汉字写法也按读音折叠，供汉字/假名混写时整句比对
  for (const [base, alt] of REC_ALT_WORDS) {
    const s = recNorm(alt), r = recNorm(base);
    if (s && r && s !== r) pairs.push([s, r]);
  }
  pairs.sort((a, b) => b[0].length - a[0].length);
  REC_FOLD = { n: LIB_WORDS.length, pairs };
  return pairs;
}
function recFoldReading(s) {
  let out = s;
  for (const [sur, rd] of recFoldMap()) {
    if (out.includes(sur)) out = out.split(sur).join(rd);
  }
  return out;
}

// recGrade 通过条件：整句原文/等价写法，或整句读音，或逐词「汉字|读音」混写。
function recGrade(l, input) {
  const got = recNorm(input);
  if (!got) return { pass: false };
  if (recAltTexts(l.text).some(t => got === recNorm(t))) return { pass: true, kind: 'ok' };
  const toks = recTokens(l);
  if (toks.length) {
    const reading = recNorm(toks.map(t => t[1] || t[0]).join(''));
    if (reading && got === reading) return { pass: true, kind: 'reading' };
    if (reading && recFoldReading(got) === reading) return { pass: true, kind: 'reading' };
    const pat = toks.map(t => {
      const alts = [...new Set([recNorm(t[0]), recNorm(t[1] || t[0])].filter(Boolean))];
      if (!alts.length) return '';
      return alts.length === 1 ? recRe(alts[0]) : '(?:' + alts.map(recRe).join('|') + ')';
    }).join('');
    if (pat) {
      try {
        if (new RegExp('^' + pat + '$').test(got)) return { pass: true, kind: 'reading' };
      } catch {}
    }
  }
  return { pass: false };
}
// 服务端兜底：kagome 把用户写的汉字折回读音（如「色々」→いろいろ），
// 与课文读音一致即判对；离线或超时则按普通错处理。
async function recReadingMatch(l, input) {
  const toks = recTokens(l);
  if (!toks.length) return false;
  const want = recNorm(toks.map(t => t[1] || t[0]).join(''));
  if (!want) return false;
  const ctl = typeof AbortController !== 'undefined' ? new AbortController() : null;
  const timer = ctl ? setTimeout(() => ctl.abort(), 4000) : 0;
  try {
    const body = await api('/recitation/reading', {
      method: 'POST',
      body: JSON.stringify({ text: input }),
      signal: ctl ? ctl.signal : undefined,
    });
    const got = recNorm(body && body.reading);
    return !!got && got === want;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}
function recEdits(a, b) {
  const n = a.length, m = b.length;
  const dp = Array.from({ length: n + 1 }, () => new Array(m + 1).fill(0));
  for (let i = 0; i <= n; i++) dp[i][0] = i;
  for (let j = 0; j <= m; j++) dp[0][j] = j;
  for (let i = 1; i <= n; i++) {
    for (let j = 1; j <= m; j++) {
      dp[i][j] = Math.min(
        dp[i - 1][j] + 1,
        dp[i][j - 1] + 1,
        dp[i - 1][j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1),
      );
    }
  }
  const ops = [];
  let i = n, j = m;
  while (i > 0 || j > 0) {
    if (i > 0 && j > 0 && dp[i][j] === dp[i - 1][j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1)) {
      ops.push({ t: a[i - 1] === b[j - 1] ? 'match' : 'sub', ai: i - 1, bi: j - 1 });
      i--; j--;
    } else if (i > 0 && dp[i][j] === dp[i - 1][j] + 1) {
      ops.push({ t: 'del', ai: i - 1 });
      i--;
    } else {
      ops.push({ t: 'ins', bi: j - 1 });
      j--;
    }
  }
  ops.reverse();
  return { dist: dp[n][m], ops };
}
// 听写对照按词对齐：每个词接受汉字面或读音两种写法，只有真正缺/错/多的部分标色。
function recDiffHTML(input, l) {
  const raw = Array.from(String(input).replace(/[\s\u3000]+/g, ''))
    .map(c => ({ c, n: recNorm(c) }))
    .filter(x => x.n);
  const N = raw.length;
  // 输入更接近某个等价写法时按它对照，避免把写对的「蟹座」标成错。
  let target = l.text, baseToks = recTokens(l);
  const gn = recNorm(input);
  let best = recEdits(Array.from(recNorm(l.text)), Array.from(gn)).dist;
  for (const alt of recAltTexts(l.text)) {
    if (alt === l.text) continue;
    const d = recEdits(Array.from(recNorm(alt)), Array.from(gn)).dist;
    if (d < best) { best = d; target = alt; baseToks = recAltTokens(l, alt); }
  }
  const mkTok = (surface, reading) => {
    const mk = str => {
      const cs = [], orig = [];
      for (const ch of Array.from(str)) {
        const n = recNorm(ch);
        if (!n) continue;
        for (const nc of Array.from(n)) { cs.push(nc); orig.push(ch); }
      }
      return { chars: cs, orig };
    };
    const variants = [];
    const sv = mk(surface);
    if (sv.chars.length) variants.push({ chars: sv.chars, orig: sv.orig });
    const rv = mk(reading);
    if (rv.chars.length && rv.chars.join('') !== sv.chars.join('')) {
      variants.push({ chars: rv.chars, orig: rv.orig });
    }
    if (!variants.length) return null;
    return { surface, variants, minLen: Math.min(...variants.map(v => v.chars.length)) };
  };
  let toks = baseToks.map(t => mkTok(t[0] || '', t[1] || '')).filter(Boolean);
  if (!toks.length) toks = Array.from(target).map(c => mkTok(c, '')).filter(Boolean);
  const T = toks.length;
  const INF = 1e9;
  const dp = Array.from({ length: T + 1 }, () => new Array(N + 1).fill(INF));
  const prev = Array.from({ length: T + 1 }, () => new Array(N + 1).fill(null));
  dp[0][0] = 0;
  const relax = (i, j, ni, nj, cost, how) => {
    const v = dp[i][j] + cost;
    if (v < dp[ni][nj]) { dp[ni][nj] = v; prev[ni][nj] = Object.assign({ i, j }, how); }
  };
  for (let i = 0; i <= T; i++) {
    for (let j = 0; j <= N; j++) {
      if (dp[i][j] >= INF) continue;
      if (i < T) relax(i, j, i + 1, j, toks[i].minLen * 10 + 1, { type: 'skipTok', ti: i });
      if (j < N) relax(i, j, i, j + 1, 6, { type: 'skipChar', bi: j });
      if (i < T) {
        const t = toks[i];
        const maxL = Math.min(N - j, Math.max(...t.variants.map(v => v.chars.length)) + 2);
        for (let L = 1; L <= maxL; L++) {
          const seg = raw.slice(j, j + L).map(x => x.n);
          t.variants.forEach((v, vi) => {
            const ed = recEdits(v.chars, seg);
            relax(i, j, i + 1, j + L, ed.dist * 10 + 1, { type: 'match', ti: i, L, vi, ed });
          });
        }
      }
    }
  }
  const actions = [];
  let i = T, j = N;
  while (i > 0 || j > 0) {
    const p = prev[i][j];
    if (!p) break;
    actions.push(p);
    i = p.i; j = p.j;
  }
  actions.reverse();
  const state = new Array(N).fill('miss');
  const missingWhole = [], missingChars = [], wrongChars = [], extraChars = [];
  for (const a of actions) {
    if (a.type === 'skipTok') {
      missingWhole.push(toks[a.ti].surface);
    } else if (a.type === 'skipChar') {
      extraChars.push(raw[a.bi].c);
    } else {
      const t = toks[a.ti];
      const v = t.variants[a.vi];
      if (a.ed.dist === 0) {
        for (let k = 0; k < a.L; k++) state[a.j + k] = 'hit';
        continue;
      }
      for (let k = 0; k < a.L; k++) state[a.j + k] = 'near';
      for (const op of a.ed.ops) {
        if (op.t === 'ins') {
          state[a.j + op.bi] = 'miss';
          extraChars.push(raw[a.j + op.bi].c);
        } else if (op.t === 'sub') {
          state[a.j + op.bi] = 'miss';
          wrongChars.push({ got: raw[a.j + op.bi].c, want: v.orig[op.ai] || v.chars[op.ai] });
        } else if (op.t === 'del') {
          missingChars.push({ ch: v.orig[op.ai] || v.chars[op.ai], token: t.surface });
        }
      }
    }
  }
  const tips = [];
  if (missingChars.length) {
    const uniq = [...new Set(missingChars.map(x => x.ch))].slice(0, 8).join('');
    const tok = [...new Set(missingChars.map(x => x.token))].slice(0, 2).join('、');
    tips.push(`少了「${esc(uniq)}」${tok ? `（${esc(tok)}）` : ''}`);
  }
  if (wrongChars.length) {
    tips.push(wrongChars.slice(0, 4).map(x => `「${esc(x.got)}」应为「${esc(x.want)}」`).join(''));
  }
  if (missingWhole.length) tips.push(`少了「${esc(missingWhole.slice(0, 4).join('、'))}」`);
  if (extraChars.length) tips.push(`多了「${esc(extraChars.slice(0, 8).join(''))}」`);
  const shown = N ? raw.map((x, k) => `<span class="${state[k]}">${esc(x.c)}</span>`).join('') : '（空）';
  return `<div class="rec-diff">你的输入：${shown}${tips.length ? `<div class="rec-diff-tip">${tips.slice(0, 2).join('；')}</div>` : ''}</div>`;
}
function recAnswerHTML(l, withNext) {
  const next = withNext
    ? '<button class="btn accent" id="rec-f-next" style="width:auto;margin-top:12px">下一句 →</button>'
    : '';
  return `<div class="rec-answer">${recJP(l)}<span class="zh">${esc(l.translation)}</span></div>${next}`;
}
async function recPost(lineNo, kind, result) {
  try {
    await api('/recitation/lessons/' + REC.lesson.id + '/progress', {
      method: 'POST',
      body: JSON.stringify({ line_no: lineNo, kind, result }),
    });
  } catch (e) { toast('进度保存失败：' + e.message); }
}

// ---- 整课音频：fetch 带 Bearer 取回 blob，缓存到 IndexedDB（离线可听） ----
function recStopLoop() {
  if (REC_RAF) cancelAnimationFrame(REC_RAF);
  REC_RAF = 0;
}
function recDropAudio() {
  recStopLoop();
  if (REC_AUDIO) {
    const a = REC_AUDIO;
    REC_AUDIO = null;
    try { a.pause(); } catch {}
    a.removeAttribute('src');
    try { a.load(); } catch {}
  }
  if (REC_AUDIO_URL) URL.revokeObjectURL(REC_AUDIO_URL);
  REC_AUDIO = null; REC_AUDIO_LESSON = null; REC_AUDIO_URL = '';
  REC_AUDIO_P = null; REC_AUDIO_P_ID = null;
  REC_STOP_AT = null;
}
async function recAudio() {
  const id = REC.lesson.id;
  if (REC_AUDIO && REC_AUDIO_LESSON === id) return REC_AUDIO;
  if (REC_AUDIO_P && REC_AUDIO_P_ID === id) return REC_AUDIO_P;
  const p = (async () => {
    let blob;
    const hit = await idbGet('lec:' + id);
    if (hit && hit.blob && hit.blob.size > 1024) {
      blob = hit.blob;
      idbPut('lec:' + id, blob);
    } else {
      const r = await fetchAuth('/api/v1/recitation/lessons/' + id + '/audio');
      if (!r.ok) throw new Error('音频获取失败 HTTP ' + r.status);
      blob = await r.blob();
      idbPut('lec:' + id, blob);
    }
    if (!REC || REC.lesson.id !== id) throw new Error('已切换课文');
    const url = URL.createObjectURL(blob);
    const a = new Audio();
    a.preload = 'auto';
    a.src = url;
    a.playbackRate = REC_RATE;
    a.addEventListener('play', () => {
      if (REC_AUDIO !== a) return;
      syncRecPlayBtn();
      recStartLoop();
    });
    a.addEventListener('pause', () => {
      if (REC_AUDIO !== a) return;
      syncRecPlayBtn();
      recStopLoop();
    });
    a.addEventListener('timeupdate', () => { if (REC_AUDIO === a) recTick(); });
    a.addEventListener('ended', () => {
      if (REC_AUDIO !== a) return;
      REC_STOP_AT = null;
      syncRecPlayBtn();
      recStopLoop();
      if (REC_MODE === 'role') recRoleAfterLine();
    });
    REC_AUDIO = a; REC_AUDIO_LESSON = id; REC_AUDIO_URL = url;
    return a;
  })();
  REC_AUDIO_P = p; REC_AUDIO_P_ID = id;
  try {
    return await p;
  } finally {
    if (REC_AUDIO_P === p) { REC_AUDIO_P = null; REC_AUDIO_P_ID = null; }
  }
}
function recStartLoop() {
  recStopLoop();
  const step = () => {
    if (!REC_AUDIO || REC_AUDIO.paused) { REC_RAF = 0; return; }
    recTick();
    REC_RAF = requestAnimationFrame(step);
  };
  REC_RAF = requestAnimationFrame(step);
}
function recIdxAt(t) {
  let lo = 0, hi = REC.lines.length - 1, ans = 0;
  while (lo <= hi) {
    const m = (lo + hi) >> 1;
    if (REC.lines[m].start_ms / 1000 <= t + 0.08) { ans = m; lo = m + 1; }
    else hi = m - 1;
  }
  return ans;
}
function recTick() {
  if (!REC || !REC_AUDIO) return;
  const a = REC_AUDIO;
  const dur = a.duration || 0;
  $('rec-bar-fill').style.width = dur ? Math.min(100, a.currentTime / dur * 100) + '%' : '0%';
  if (REC_STOP_AT != null && a.currentTime >= REC_STOP_AT) {
    REC_STOP_AT = null;
    a.pause();
    if (REC_MODE === 'role') recRoleAfterLine();
    return;
  }
  if (REC_MODE === 'listen' && !a.paused) {
    const idx = recIdxAt(a.currentTime);
    if (idx !== REC_IDX) {
      REC_IDX = idx;
      recSavePos();
      recHighlight(idx, true);
    }
  }
}
function syncRecPlayBtn() {
  const b = $('rec-play');
  if (b) b.textContent = REC_AUDIO && !REC_AUDIO.paused ? '⏸' : '▶';
  const rb = $('rec-role-audio');
  if (rb) rb.textContent = REC_AUDIO && !REC_AUDIO.paused ? '⏸ 暂停' : '▶ 听原声';
}
function recHighlight(idx, scroll) {
  const box = $('rec-lines');
  const els = box.querySelectorAll('.rec-line');
  els.forEach((el, i) => el.classList.toggle('on', i === idx));
  if (scroll && REC_FOLLOW && els[idx]) {
    els[idx].scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
}

// ---- 播放控制 ----
function recLineStop(idx, a) {
  const l = REC.lines[idx];
  let stop = (l.end_ms + 80) / 1000;
  const nl = REC.lines[idx + 1];
  if (nl && nl.start_ms / 1000 < stop) {
    stop = Math.max(l.start_ms / 1000 + 0.15, nl.start_ms / 1000 - 0.02);
  }
  return Math.min(stop, (a && a.duration) || Infinity);
}
async function recPlayAt(idx, continuous, silent) {
  if (!REC) return;
  idx = Math.max(0, Math.min(REC.lines.length - 1, idx));
  REC_IDX = idx;
  REC_FOLLOW = true;
  recSavePos();
  if (REC_MODE === 'read' || REC_MODE === 'listen') recHighlight(idx, true);
  let a;
  try {
    a = await recAudio();
  } catch (e) {
    if (!silent) toast(e.message);
    return;
  }
  if (!REC || REC.lines[idx] !== recLine()) return; // 等待期间用户已切到别的句子
  const l = REC.lines[idx];
  REC_STOP_AT = continuous ? null : recLineStop(idx, a);
  a.currentTime = l.start_ms / 1000;
  a.playbackRate = REC_RATE;
  try {
    await playUnlock(a);
    syncRecPlayBtn();
  } catch (e) {
    if (!silent) toast('播放失败：' + e.message);
  }
}
async function recResumeLine() {
  const l = recLine();
  if (!l) return;
  let a;
  try { a = await recAudio(); } catch (e) { toast(e.message); return; }
  const t = a.currentTime * 1000;
  if (t >= l.start_ms - 100 && t < l.end_ms) {
    REC_STOP_AT = recLineStop(REC_IDX, a);
    try { await playUnlock(a); syncRecPlayBtn(); } catch (e) { toast('播放失败：' + e.message); }
    return;
  }
  recPlayAt(REC_IDX, false);
}
async function recToggle() {
  if (!REC) return;
  if (REC_AUDIO && !REC_AUDIO.paused) { REC_AUDIO.pause(); return; }
  if (REC_MODE === 'listen') {
    if (REC_AUDIO && REC_AUDIO.currentTime > 0 && !REC_AUDIO.ended) {
      REC_STOP_AT = null;
      REC_FOLLOW = true;
      try { await playUnlock(REC_AUDIO); syncRecPlayBtn(); } catch (e) { toast('播放失败：' + e.message); }
      return;
    }
    recPlayAt(REC_IDX, true);
    return;
  }
  recResumeLine();
}
function recPrev() {
  if (REC) recPlayAt(Math.max(0, REC_IDX - 1), REC_MODE === 'listen');
}
function recNext() {
  if (REC) recPlayAt(Math.min(REC.lines.length - 1, REC_IDX + 1), REC_MODE === 'listen');
}

// ---- 列表与页面 ----
function recRenderList() {
  const box = $('rec-list');
  if (!REC_LIST.length) {
    box.innerHTML = '<p class="hint">还没有课文内容。</p>';
    $('rec-list-hint').textContent = '';
    return;
  }
  box.innerHTML = REC_LIST.map(it => {
    const pctR = it.line_count ? Math.round(it.recited_count / it.line_count * 100) : 0;
    const pctD = it.line_count ? Math.round(it.dict_count / it.line_count * 100) : 0;
    return `<button class="rec-item" data-id="${esc(it.id)}">
      <div class="rec-item-top">
        <span class="rec-item-title">${esc(recTitle(it))}</span>
        <span class="rec-item-meta">${recFmt(it.duration_ms)} · ${it.line_count} 句</span>
      </div>
      <div class="rec-stat"><span>背诵</span><div class="track"><i class="recited" style="width:${pctR}%"></i></div><span>${it.recited_count}/${it.line_count}</span></div>
      <div class="rec-stat"><span>听写</span><div class="track"><i class="dict" style="width:${pctD}%"></i></div><span>${it.dict_count}/${it.line_count}</span></div>
    </button>`;
  }).join('');
  $('rec-list-hint').textContent = '进度按行记在账号里，换设备登录也会同步。';
}
async function loadRecCount() {
  try {
    REC_LIST = (await api('/recitation/lessons')).items || [];
    const n = REC_LIST.reduce((s, x) => s + x.line_count, 0);
    $('rec-desc').textContent = REC_LIST.length
      ? `共 ${REC_LIST.length} 课 · ${n} 句 · 听读、背诵、听写、对戏，还能用消失术、中译日、通关赛挑战记忆。`
      : '还没有课文内容。';
  } catch {}
}
async function goRecite() {
  stopPlay();
  go('recite');
  const box = $('rec-list');
  if (!REC_LIST.length) box.innerHTML = '<p class="hint">载入中…</p>';
  try {
    REC_LIST = (await api('/recitation/lessons')).items || [];
  } catch (e) {
    if (!REC_LIST.length) box.innerHTML = `<p class="hint">载入失败：${esc(e.message)}</p>`;
    return;
  }
  recRenderList();
}
async function openRec(id) {
  stopPlay();
  let d;
  try {
    d = await api('/recitation/lessons/' + id);
  } catch (e) { toast(e.message); return; }
  if (REC && REC.lesson.id !== id) recDropAudio();
  REC = d;
  const saved = parseInt(localStorage.getItem('ts_rec_pos_' + id) || '0', 10);
  const idx = REC.lines.findIndex(l => l.line_no === saved);
  REC_IDX = idx >= 0 ? idx : 0;
  REC_REVEAL = false;
  REC_DICT_TRIES = 0;
  REC_DICT_LOCK = false;
  REC_ROLE = localStorage.getItem('ts_rec_role_' + id) || '';
  REC_ROLE_ACTIVE = false;
  REC_ROLE_FINISHED = false;
  REC_ROLE_PICKING = false;
  REC_ROLE_REVEAL = false;
  REC_ROLE_DONE = 0;
  REC_ROLE_HINTS = 0;
  REC_ROLE_HINTED = new Set();
  clearTimeout(REC_ROLE_TIMER);
  $('rec-title').textContent = recTitle(d.lesson);
  go('recite-play');
  setRecMode('read');
  window.scrollTo(0, 0);
  recAudio().catch(() => {});
}
function recLeave() {
  clearTimeout(REC_DICT_TIMER);
  clearTimeout(REC_ROLE_TIMER);
  REC_STOP_AT = null;
  recStopLoop();
  if (REC_AUDIO) { try { REC_AUDIO.pause(); } catch {} }
  document.body.classList.remove('rec-playing');
  const p = $('rec-player');
  if (p) p.hidden = true;
  if (REC) {
    const it = REC_LIST.find(x => x.id === REC.lesson.id);
    if (it) {
      it.recited_count = REC.lines.filter(l => l.recited).length;
      it.dict_count = REC.lines.filter(l => l.dict_ok || l.dict_reading).length;
      recRenderList();
    }
  }
}

// ---- 听读 / 连听 ----
function renderRecLines() {
  if (!REC) return;
  $('rec-lines').innerHTML = REC.lines.map((l, i) => {
    const marks = (l.recited ? '<span class="done">背✓</span>' : '') +
      ((l.dict_ok || l.dict_reading) ? '<span class="done dict">听✓</span>' : '');
    return `<div class="rec-line" data-idx="${i}">
      <div class="n">${i + 1}</div>
      <div class="jp">${recJP(l)}${marks}<div class="zh">${esc(l.translation)}</div></div>
    </div>`;
  }).join('');
}
function setRecMode(mode) {
  if (!REC) return;
  REC_MODE = mode;
  REC_REVEAL = false;
  REC_DICT_TRIES = 0;
  REC_DICT_LOCK = false;
  REC_DICT_CHECKING = false;
  REC_ROLE_REVEAL = false;
  clearTimeout(REC_DICT_TIMER);
  clearTimeout(REC_ROLE_TIMER);
  document.querySelectorAll('#rec-modes button').forEach(b => b.classList.toggle('on', b.dataset.mode === mode));
  $('rec-mode-desc').textContent = REC_DESC[mode] || '';
  const listMode = mode === 'read' || mode === 'listen';
  $('rec-lines').hidden = !listMode;
  $('rec-focus').hidden = listMode;
  $('rec-player').hidden = !listMode;
  document.body.classList.toggle('rec-playing', listMode);
  if (REC_AUDIO) { try { REC_AUDIO.pause(); } catch {} }
  REC_STOP_AT = null;
  if (listMode) {
    renderRecLines();
    $('rec-lines').querySelectorAll('.rec-line').forEach(el => el.classList.toggle('showzh', mode === 'read'));
    recHighlight(REC_IDX, false);
    syncRecPlayBtn();
  } else if (mode === 'role') {
    if (REC_ROLE_FINISHED) {
      renderRecRoleDone();
    } else if (REC_ROLE_PICKING) {
      renderRoleSelect();
    } else if (recRoleValid()) {
      if (REC_ROLE_ACTIVE) { renderRoleLine(); recRoleAutoPlay(); }
      else recRoleBegin();
    } else {
      renderRoleSelect();
    }
  } else if (mode === 'recite') {
    renderRecite();
    recPlayAt(REC_IDX, false, true);
  } else if (mode === 'cloze') {
    recClozeBegin();
  } else if (mode === 'zh2jp') {
    recZhBegin();
  } else if (mode === 'run') {
    recRunBegin();
  } else {
    renderDict();
    recPlayAt(REC_IDX, false, true);
  }
}

// ---- 背诵 ----
function recHidden(l) {
  const chars = Array.from(l.text || '');
  if (!chars.length) return '';
  const hint = chars.find(c => !/[\s、。，．！？!?「」『』（）()…]/.test(c)) || chars[0];
  const rest = Math.max(0, chars.length - 1);
  const marks = '○'.repeat(Math.min(rest, 18)) + (rest > 18 ? '…' : '');
  return esc(hint) + (marks ? `<span class="blank-hint">${marks}</span>` : '');
}
function renderRecite() {
  const l = recLine();
  if (!l) return;
  REC_REVEAL = false;
  $('rec-focus').innerHTML = `
    <div class="rec-focus">
      <div class="rec-focus-top"><span>第 ${REC_IDX + 1} / ${REC.lines.length} 句 · 背诵</span><span>${l.recited ? '已背 ✓' : ''}</span></div>
      <div class="rec-focus-jp" id="rec-focus-jp">${recHidden(l)}</div>
      <div class="rec-focus-zh hide" id="rec-focus-zh">${esc(l.translation)}</div>
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-f-play">▶ 播放本句</button>
        <button class="btn ghost" id="rec-f-show">看答案</button>
        <button class="btn accent wide" id="rec-f-done">会背了</button>
      </div>
    </div>`;
  $('rec-f-play').onclick = () => recPlayAt(REC_IDX, false);
  $('rec-f-show').onclick = recToggleAnswer;
  $('rec-f-done').onclick = recReciteDone;
}
function recToggleAnswer() {
  const l = recLine();
  if (!l) return;
  REC_REVEAL = !REC_REVEAL;
  $('rec-focus-jp').innerHTML = REC_REVEAL ? recJP(l) : recHidden(l);
  $('rec-focus-zh').classList.toggle('hide', !REC_REVEAL);
  $('rec-f-show').textContent = REC_REVEAL ? '隐藏答案' : '看答案';
}
function recReciteDone() {
  const l = recLine();
  if (!l) return;
  if (!l.recited) {
    l.recited = true;
    recPost(l.line_no, 'recite', '');
  }
  recFocusAdvance();
}

// ---- 听写 ----
function renderDict() {
  const l = recLine();
  if (!l) return;
  REC_DICT_TRIES = 0;
  REC_DICT_LOCK = false;
  REC_DICT_CHECKING = false;
  $('rec-focus').innerHTML = `
    <div class="rec-focus">
      <div class="rec-focus-top">
        <span>第 ${REC_IDX + 1} / ${REC.lines.length} 句 · 听写</span>
        <span class="rec-dict-zh" id="rec-dict-zh" role="button" tabindex="0" aria-label="点一下显示中文译文"><span class="rec-dict-zh-label">中文</span><span class="rec-dict-zh-text">${esc(l.translation)}</span></span>
        <span class="rec-dict-tools">
          <span id="rec-dict-mark">${recMark(l)}</span>
          <button class="btn ghost rec-top-btn" id="rec-f-play">▶ 播放本句</button>
        </span>
      </div>
      <div class="rec-dict">
        <input id="rec-input" type="text" lang="ja" placeholder="听音频，输入这句日语…" autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false">
        <button class="btn accent" id="rec-check">检查</button>
      </div>
      <div class="rec-feedback" id="rec-feedback"></div>
      <div id="rec-diff"></div>
      <div id="rec-answer"></div>
    </div>`;
  $('rec-f-play').onclick = () => recPlayAt(REC_IDX, false);
  $('rec-check').onclick = recCheckDict;
  const zh = $('rec-dict-zh');
  const toggleZh = () => {
    zh.classList.toggle('show');
    zh.closest('.rec-focus-top').classList.toggle('zh-open', zh.classList.contains('show'));
  };
  zh.onclick = toggleZh;
  zh.onkeydown = e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleZh(); }
  };
  $('rec-input').addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); recCheckDict(); }
  });
}
async function recCheckDict() {
  if (!REC || REC_MODE !== 'dict' || REC_DICT_LOCK || REC_DICT_CHECKING) return;
  const inp = $('rec-input');
  if (!inp) return;
  const val = inp.value.trim();
  if (!val) { toast('先输入听到的句子'); inp.focus(); return; }
  const l = recLine();
  if (!l) return;
  let g = recGrade(l, val);
  // 本地不认识的汉字写法，交给服务端折读音再比一次（课文写假名的情况）
  if (!g.pass && /[\u4e00-\u9fff々]/.test(val)) {
    REC_DICT_CHECKING = true;
    const btn = $('rec-check');
    if (btn) { btn.disabled = true; btn.textContent = '检查中…'; }
    try {
      if (recLine() === l && REC_MODE === 'dict' && await recReadingMatch(l, val)) {
        g = { pass: true, kind: 'reading' };
      }
    } finally {
      REC_DICT_CHECKING = false;
      const b = $('rec-check');
      if (b) { b.disabled = false; b.textContent = '检查'; }
    }
    if (!REC || REC_MODE !== 'dict' || recLine() !== l || REC_DICT_LOCK) return;
  }
  if (g.pass) recDictPass(l, g.kind, inp);
  else recDictFail(l, val, inp);
}
function recDictPass(l, kind, inp) {
  REC_DICT_LOCK = true;
  if (!l.dict_ok && !l.dict_reading) {
    if (kind === 'ok') l.dict_ok = true; else l.dict_reading = true;
    recPost(l.line_no, 'dict', kind);
  }
  inp.disabled = true;
  const fb = $('rec-feedback');
  fb.className = 'rec-feedback ok';
  fb.textContent = kind === 'ok' ? '正确！' : '正确（读音一致）';
  $('rec-diff').innerHTML = '';
  $('rec-answer').innerHTML = recAnswerHTML(l, true);
  $('rec-dict-zh')?.remove();
  $('rec-dict-mark').textContent = recMark(l);
  $('rec-f-next').onclick = () => { clearTimeout(REC_DICT_TIMER); recFocusAdvance(); };
  REC_DICT_TIMER = setTimeout(recFocusAdvance, 2400);
}
function recDictFail(l, val, inp) {
  REC_DICT_TRIES++;
  const fb = $('rec-feedback');
  fb.className = 'rec-feedback bad';
  fb.textContent = REC_DICT_TRIES >= 2
    ? '还不完全对。答案已给出，可以照着打一遍。'
    : '还不完全对，再听一遍试试。';
  $('rec-diff').innerHTML = recDiffHTML(val, l);
  if (REC_DICT_TRIES >= 2 && !$('rec-answer').innerHTML) {
    $('rec-answer').innerHTML = recAnswerHTML(l, true);
    $('rec-dict-zh')?.remove();
    $('rec-f-next').onclick = () => recFocusAdvance();
  }
  inp.select();
  recPlayAt(REC_IDX, false, true);
}
function recFocusAdvance() {
  clearTimeout(REC_DICT_TIMER);
  if (!REC) return;
  if (REC_IDX >= REC.lines.length - 1) { renderRecDone(); return; }
  REC_IDX++;
  recSavePos();
  REC_REVEAL = false;
  REC_DICT_TRIES = 0;
  REC_DICT_LOCK = false;
  REC_DICT_CHECKING = false;
  if (REC_MODE === 'recite') {
    renderRecite();
  } else {
    renderDict();
  }
  recPlayAt(REC_IDX, false, true);
}
function renderRecDone() {
  if (!REC) return;
  const total = REC.lines.length;
  const recd = REC.lines.filter(l => l.recited).length;
  const dict = REC.lines.filter(l => l.dict_ok || l.dict_reading).length;
  $('rec-focus').innerHTML = `
    <div class="rec-focus">
      <div class="rec-focus-top"><span>第 ${total} / ${total} 句</span><span>完成</span></div>
      <div class="rec-focus-jp">本课练习完成。</div>
      <div class="rec-focus-zh">已背 ${recd} / ${total} 句 · 听写通过 ${dict} / ${total} 句。</div>
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-done-again">从头再练</button>
        <button class="btn accent wide" id="rec-done-list">返回列表</button>
      </div>
    </div>`;
  $('rec-done-again').onclick = () => {
    REC_IDX = 0;
    recSavePos();
    if (REC_MODE === 'recite') renderRecite(); else { REC_DICT_TRIES = 0; renderDict(); }
    recPlayAt(0, false, true);
  };
  $('rec-done-list').onclick = () => goBack();
}

// ---- 对戏 ----
// 角色取自教材会话标注：单人行给你演；对方行自动播下一句；双人合说行整句自动播。
function recRoleKind(l) {
  const sp = Array.isArray(l.speakers) ? l.speakers : [];
  if (!sp.length) return 'narrator';
  if (sp.length > 1) return sp.includes(REC_ROLE) ? 'both' : 'other';
  return sp[0] === REC_ROLE ? 'mine' : 'other';
}
function recRoleValid() {
  return !!(REC && REC_ROLE &&
    REC.lines.some(l => Array.isArray(l.speakers) && l.speakers.includes(REC_ROLE)));
}
function recRoleBegin() {
  if (!REC) return;
  REC_ROLE_ACTIVE = true;
  REC_ROLE_FINISHED = false;
  REC_ROLE_PICKING = false;
  REC_ROLE_DONE = 0;
  REC_ROLE_HINTS = 0;
  REC_ROLE_HINTED = new Set();
  REC_ROLE_REVEAL = false;
  REC_ROLE_TOTAL = REC.lines.filter(l => recRoleKind(l) === 'mine').length;
  REC_IDX = 0;
  recSavePos();
  renderRoleLine();
  recRoleAutoPlay();
}
function recRolePick(who) {
  if (!REC) return;
  REC_ROLE = who;
  localStorage.setItem('ts_rec_role_' + REC.lesson.id, who);
  recRoleBegin();
}
function recRoleStopAudio() {
  clearTimeout(REC_ROLE_TIMER);
  REC_STOP_AT = null;
  if (REC_AUDIO) { try { REC_AUDIO.pause(); } catch {} }
}
function recRoleSelectUI() {
  recRoleStopAudio();
  REC_ROLE_ACTIVE = false;
  REC_ROLE_FINISHED = false;
  REC_ROLE_PICKING = true;
  renderRoleSelect();
}
function renderRoleSelect() {
  if (!REC) return;
  REC_ROLE_REVEAL = false;
  const counts = new Map();
  REC.lines.forEach(l => {
    const sp = Array.isArray(l.speakers) ? l.speakers : [];
    sp.forEach(who => {
      if (!counts.has(who)) counts.set(who, { own: 0, both: 0 });
      const c = counts.get(who);
      if (sp.length === 1) c.own++; else c.both++;
    });
  });
  const roles = [...counts.entries()]
    .filter(([who]) => who !== '皆')
    .sort((a, b) => b[1].own - a[1].own || a[0].localeCompare(b[0]));
  if (!roles.length) {
    $('rec-focus').innerHTML = `
      <div class="rec-focus">
        <div class="rec-focus-top"><span>对戏</span></div>
        <div class="rec-role-title">这一课还没有角色标注</div>
        <div class="rec-role-intro">先用听读或背诵模式练习吧。</div>
      </div>`;
    return;
  }
  const scenes = [...new Set(REC.lines.map(l => l.scene).filter(Boolean))];
  $('rec-focus').innerHTML = `
    <div class="rec-focus">
      <div class="rec-focus-top"><span>对戏 · 选角色</span><span>${REC.lines.length} 句</span></div>
      <div class="rec-role-title">你想演谁？</div>
      <div class="rec-role-intro">对方台词自动播放；轮到你时只给首字提示，说完点「接上了」。双方合说的句子自动播放，不计入你的台词数。</div>
      <div class="rec-role-list">
        ${roles.map(([who, c]) => `<button class="rec-role-pick" data-who="${esc(who)}">
          <span class="rec-role-name">${esc(who)}</span>
          <span class="rec-role-count">${c.own} 句台词${c.both ? ` · ${c.both} 句合说` : ''}</span>
        </button>`).join('')}
      </div>
      ${scenes.length ? `<div class="rec-role-scenes">场景：${scenes.map(esc).join(' · ')}</div>` : ''}
    </div>`;
  $('rec-focus').querySelectorAll('.rec-role-pick').forEach(b => {
    b.onclick = () => recRolePick(b.dataset.who);
  });
}
function renderRoleLine() {
  const l = recLine();
  if (!l) return;
  const kind = recRoleKind(l);
  const sp = Array.isArray(l.speakers) ? l.speakers : [];
  const showScene = l.scene && (REC_IDX === 0 || REC.lines[REC_IDX - 1].scene !== l.scene);
  let who = '旁白';
  if (kind === 'mine') who = '你 · ' + REC_ROLE;
  else if (kind === 'both') who = '双方 · ' + sp.join(' / ');
  else if (sp.length) who = '对方 · ' + sp.join(' / ');
  const mine = kind === 'mine';
  const full = !mine || REC_ROLE_REVEAL;
  const actions = mine
    ? `<button class="btn ghost" id="rec-role-audio">▶ 听原声</button>
       <button class="btn ghost" id="rec-role-show">${REC_ROLE_REVEAL ? '收起答案' : '看答案'}</button>
       <button class="btn accent wide" id="rec-role-done">接上了 →</button>`
    : `<button class="btn ghost" id="rec-role-audio">▶ 听原声</button>
       <button class="btn accent wide" id="rec-role-next">下一句 →</button>`;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-role">
      <div class="rec-focus-top">
        <span>第 ${REC_IDX + 1} / ${REC.lines.length} 句 · 对戏</span>
        <button class="rec-role-switch" id="rec-role-switch">你演 ${esc(REC_ROLE)} · 换角色</button>
      </div>
      ${showScene ? `<div class="rec-role-scene">${esc(l.scene)}</div>` : ''}
      <div class="rec-role-who ${kind}">${esc(who)}${l.recited ? '<span class="rec-role-mark">已背 ✓</span>' : ''}</div>
      <div class="rec-focus-jp" id="rec-role-jp">${full ? recJP(l) : recHidden(l)}</div>
      <div class="rec-focus-zh${full ? '' : ' hide'}" id="rec-role-zh">${esc(l.translation)}</div>
      <div class="rec-focus-actions">${actions}</div>
      <div class="rec-role-progress">本场台词 ${REC_ROLE_DONE} / ${REC_ROLE_TOTAL}${REC_ROLE_HINTS ? ` · 看答案 ${REC_ROLE_HINTS} 次` : ''}</div>
    </div>`;
  $('rec-role-switch').onclick = recRoleSelectUI;
  $('rec-role-audio').onclick = recRoleAudioToggle;
  if (mine) {
    $('rec-role-show').onclick = recRoleToggleAnswer;
    $('rec-role-done').onclick = recRoleDone;
  } else {
    $('rec-role-next').onclick = () => recRoleNext();
  }
  syncRecPlayBtn();
}
function recRoleAutoPlay() {
  if (!REC || REC_MODE !== 'role') return;
  const l = recLine();
  if (!l || recRoleKind(l) === 'mine') { syncRecPlayBtn(); return; }
  recPlayAt(REC_IDX, false, true);
}
function recRoleAfterLine() {
  if (!REC || REC_MODE !== 'role') return;
  const l = recLine();
  if (!l || recRoleKind(l) === 'mine') return;
  clearTimeout(REC_ROLE_TIMER);
  REC_ROLE_TIMER = setTimeout(() => {
    if (!REC || REC_MODE !== 'role' || recRoleKind(recLine()) === 'mine') return;
    recRoleNext();
  }, 700);
}
function recRoleNext() {
  clearTimeout(REC_ROLE_TIMER);
  if (!REC || REC_MODE !== 'role') return;
  if (REC_IDX >= REC.lines.length - 1) { renderRecRoleDone(); return; }
  if (REC_AUDIO && !REC_AUDIO.paused) { try { REC_AUDIO.pause(); } catch {} }
  REC_STOP_AT = null;
  REC_IDX++;
  recSavePos();
  REC_ROLE_REVEAL = false;
  renderRoleLine();
  recRoleAutoPlay();
}
function recRoleAudioToggle() {
  if (!REC || REC_MODE !== 'role') return;
  if (REC_AUDIO && !REC_AUDIO.paused) { REC_AUDIO.pause(); return; }
  recResumeLine();
}
function recRoleToggleAnswer() {
  const l = recLine();
  if (!l || REC_MODE !== 'role' || recRoleKind(l) !== 'mine') return;
  REC_ROLE_REVEAL = !REC_ROLE_REVEAL;
  $('rec-role-jp').innerHTML = REC_ROLE_REVEAL ? recJP(l) : recHidden(l);
  $('rec-role-zh').classList.toggle('hide', !REC_ROLE_REVEAL);
  $('rec-role-show').textContent = REC_ROLE_REVEAL ? '收起答案' : '看答案';
  if (REC_ROLE_REVEAL && !REC_ROLE_HINTED.has(l.line_no)) {
    REC_ROLE_HINTED.add(l.line_no);
    REC_ROLE_HINTS++;
    const pg = $('rec-focus').querySelector('.rec-role-progress');
    if (pg) pg.textContent = `本场台词 ${REC_ROLE_DONE} / ${REC_ROLE_TOTAL} · 看答案 ${REC_ROLE_HINTS} 次`;
  }
}
function recRoleDone() {
  const l = recLine();
  if (!l || REC_MODE !== 'role' || recRoleKind(l) !== 'mine') return;
  REC_ROLE_DONE++;
  if (!l.recited) {
    l.recited = true;
    recPost(l.line_no, 'recite', '');
  }
  recRoleNext();
}
function renderRecRoleDone() {
  if (!REC) return;
  recRoleStopAudio();
  REC_ROLE_FINISHED = true;
  REC_ROLE_ACTIVE = false;
  const total = REC_ROLE_TOTAL;
  const done = REC_ROLE_DONE;
  const hints = REC_ROLE_HINTS;
  const mixed = REC.lines.filter(l => recRoleKind(l) === 'both').length;
  const perfect = total > 0 && done >= total && hints === 0;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-role-done">
      <div class="rec-focus-top"><span>对戏 · ${esc(REC_ROLE)}</span><span>完成</span></div>
      <div class="rec-role-curtain">本场杀青</div>
      <div class="rec-role-stats">
        <span>你的台词 <b>${done} / ${total}</b> 句</span>
        <span>看答案 <b>${hints}</b> 次</span>
        <span>双方合说 <b>${mixed}</b> 句</span>
      </div>
      ${perfect ? '<div class="rec-role-perfect">零提示通关，台词全接上了。</div>' : ''}
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-role-again">再对一场</button>
        <button class="btn ghost" id="rec-role-change">换角色</button>
        <button class="btn accent wide" id="rec-role-back">返回列表</button>
      </div>
    </div>`;
  $('rec-role-again').onclick = recRoleBegin;
  $('rec-role-change').onclick = recRoleSelectUI;
  $('rec-role-back').onclick = () => goBack();
}

// ---- 消失术 ----
// 每句从全文开始，每点一次「背对了」多遮住 1/3 的词，遮到只剩首字假名后过关。
// 点遮住的词可偷看整词，但本句星星 -1（3 星起）。
function recClozeResetLine() {
  REC_CLOZE_STEP = 0;
  REC_CLOZE_HIDDEN = new Set();
  REC_CLOZE_PEEK = new Set();
  REC_CLOZE_STARS = 3;
  REC_CLOZE_WORDS = REC ? recWords(recLine()) : [];
}
function recClozeBegin() {
  REC_CLOZE_TOTAL = 0;
  REC_CLOZE_PERFECT = 0;
  recClozeResetLine();
  renderCloze();
  recPlayAt(REC_IDX, false, true);
}
function recClozeWordHTML(t, hidden) {
  if (hidden) {
    return `<span class="rec-cloze-word" data-tok="${t.i}" title="点一下偷看">${esc(recFirstKana(t.t))}<span class="rec-cloze-dots">…</span></span>`;
  }
  const cls = REC_CLOZE_PEEK.has(t.i) ? 'rec-cloze-word rec-cloze-peek' : 'rec-cloze-word';
  return `<span class="${cls}">${rubyHTML([t.t], true)}</span>`;
}
function recClozeHTML(l) {
  const toks = recTokens(l);
  if (!toks.length) return esc(l.text);
  const words = new Set(REC_CLOZE_WORDS);
  return toks.map((t, i) => {
    if (!t[0] || recIsPunct(t[0])) return esc(t[0]);
    const hidden = words.has(i) && REC_CLOZE_HIDDEN.has(i) && !REC_CLOZE_PEEK.has(i);
    return recClozeWordHTML({ t, i }, hidden);
  }).join('');
}
function recClozeStarsHTML() {
  return `<span class="rec-cloze-stars">${[0, 1, 2].map(i => `<i class="${i < REC_CLOZE_STARS ? 'on' : ''}">★</i>`).join('')}</span>`;
}
function renderCloze() {
  const l = recLine();
  if (!l) return;
  const stepTxt = REC_CLOZE_STEP === 0 ? '全文' : (REC_CLOZE_STEP >= 3 ? '只剩首字' : `已遮 ${REC_CLOZE_STEP} / 3`);
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-cloze">
      <div class="rec-focus-top"><span>第 ${REC_IDX + 1} / ${REC.lines.length} 句 · 消失术</span>${recClozeStarsHTML()}</div>
      <div class="rec-focus-jp" id="rec-cloze-jp">${recClozeHTML(l)}</div>
      <div class="rec-focus-zh">${esc(l.translation)}</div>
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-cloze-play">▶ 播放本句</button>
        <button class="btn accent wide" id="rec-cloze-done">${REC_CLOZE_STEP >= 3 ? '背对了，下一句 →' : '背对了 →'}</button>
      </div>
      <div class="rec-drill-progress"><span>本句 ${stepTxt}</span><span>本课已得 ★ ${REC_CLOZE_TOTAL}</span></div>
    </div>`;
  $('rec-cloze-play').onclick = () => recPlayAt(REC_IDX, false);
  $('rec-cloze-done').onclick = recClozeStep;
  $('rec-cloze-jp').querySelectorAll('.rec-cloze-word').forEach(el => {
    el.onclick = () => recClozePeek(+el.dataset.tok);
  });
}
function recClozePeek(tokIdx) {
  if (!REC || REC_MODE !== 'cloze') return;
  if (!REC_CLOZE_HIDDEN.has(tokIdx) || REC_CLOZE_PEEK.has(tokIdx)) return;
  REC_CLOZE_PEEK.add(tokIdx);
  if (REC_CLOZE_STARS > 0) REC_CLOZE_STARS--;
  renderCloze();
}
function recClozeStep() {
  if (!REC || REC_MODE !== 'cloze') return;
  const l = recLine();
  if (!l) return;
  if (REC_CLOZE_STEP < 3 && REC_CLOZE_WORDS.length) {
    REC_CLOZE_STEP++;
    const target = Math.ceil(REC_CLOZE_WORDS.length * REC_CLOZE_STEP / 3);
    const rest = REC_CLOZE_WORDS.filter(i => !REC_CLOZE_HIDDEN.has(i));
    for (let i = rest.length - 1; i > 0; i--) {
      const j = Math.floor(Math.random() * (i + 1));
      [rest[i], rest[j]] = [rest[j], rest[i]];
    }
    for (const i of rest) {
      if (REC_CLOZE_HIDDEN.size >= target) break;
      REC_CLOZE_HIDDEN.add(i);
    }
    renderCloze();
    return;
  }
  REC_CLOZE_TOTAL += REC_CLOZE_STARS;
  if (REC_CLOZE_STARS === 3) REC_CLOZE_PERFECT++;
  if (!l.recited) {
    l.recited = true;
    recPost(l.line_no, 'recite', '');
  }
  if (REC_IDX >= REC.lines.length - 1) { renderClozeDone(); return; }
  REC_IDX++;
  recSavePos();
  recClozeResetLine();
  renderCloze();
  recPlayAt(REC_IDX, false, true);
}
function renderClozeDone() {
  if (!REC) return;
  const max = REC.lines.length * 3;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-role-done">
      <div class="rec-focus-top"><span>消失术</span><span>完成</span></div>
      <div class="rec-role-curtain">课文消失完毕</div>
      <div class="rec-role-stats">
        <span>获得星星 <b>${REC_CLOZE_TOTAL} / ${max}</b></span>
        <span>满星句子 <b>${REC_CLOZE_PERFECT}</b> 句</span>
      </div>
      ${REC_CLOZE_PERFECT === REC.lines.length ? '<div class="rec-role-perfect">每句都拿满星，记忆很稳。</div>' : ''}
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-cloze-again">再来一遍</button>
        <button class="btn accent wide" id="rec-cloze-back">返回列表</button>
      </div>
    </div>`;
  $('rec-cloze-again').onclick = () => {
    REC_IDX = 0;
    recSavePos();
    recClozeBegin();
  };
  $('rec-cloze-back').onclick = () => goBack();
}

// ---- 中译日 ----
// 只给中文；提示四级：首字 → 每词首字 → 关键词 → 全句。不自动播放音频，避免一上来听到答案。
const REC_ZH_STOP = new Set(['は', 'が', 'を', 'に', 'で', 'と', 'も', 'の', 'か', 'ね', 'よ', 'な', 'です', 'ます', 'ました', 'ません', 'でした', 'では', 'じゃ', 'て', 'た', 'だ', 'から', 'まで', 'より', 'へ', 'や', 'ば', 'ので', 'のに', 'けど', 'でも', 'そして', 'それ', 'これ', 'あれ', 'どれ', 'ここ', 'そこ', 'あそこ', 'ええ', 'はい', 'いいえ']);
function recZhKeywords(l) {
  const toks = recTokens(l).filter(t => t[0] && !recIsPunct(t[0]));
  if (!toks.length) return [];
  const picks = toks.filter(t => /[\u4e00-\u9fff]/.test(t[0]) ||
    (Array.from(t[0]).length >= 2 && !REC_ZH_STOP.has(t[0]))).map(t => t[0]);
  const uniq = [...new Set(picks)].slice(0, 4);
  return uniq.length ? uniq : [toks[0][0]];
}
function recZhHintHTML(l) {
  if (REC_ZH_LVL === 0) return '<div class="rec-zh-hint-empty">先出声说一遍；卡住再点「提示」，一级一级来。</div>';
  const toks = recTokens(l);
  if (REC_ZH_LVL === 1) {
    const t = toks.find(x => x[0] && !recIsPunct(x[0]));
    return `<div class="rec-zh-hint-label">首字</div><div class="rec-zh-hint-lvl">${esc(recFirstChar(t))}…</div>`;
  }
  if (REC_ZH_LVL === 2) {
    const chars = recWords(l).map(i => recFirstChar(toks[i]));
    return `<div class="rec-zh-hint-label">每词首字</div><div class="rec-zh-hint-lvl">${chars.map(esc).join(' ')}</div>`;
  }
  if (REC_ZH_LVL === 3) {
    const kw = recZhKeywords(l);
    return `<div class="rec-zh-hint-label">关键词</div><div class="rec-zh-kw">${kw.map(k => `<span>${esc(k)}</span>`).join('')}</div>`;
  }
  return `<div class="rec-zh-hint-label">全句</div><div class="rec-focus-jp rec-zh-answer">${recJP(l)}</div>`;
}
function recZhBegin() {
  REC_ZH_LVL = 0;
  REC_ZH_USED_THIS = false;
  REC_ZH_CLEAN = 0;
  REC_ZH_USED = 0;
  renderZh2Jp();
}
function renderZh2Jp() {
  const l = recLine();
  if (!l) return;
  const lvl = REC_ZH_LVL;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-zh">
      <div class="rec-focus-top"><span>第 ${REC_IDX + 1} / ${REC.lines.length} 句 · 中译日</span><span>${REC_ZH_USED_THIS ? '本句已用提示' : '本句零提示'}</span></div>
      <div class="rec-zh-prompt">${esc(l.translation)}</div>
      <div class="rec-zh-sub">把这句话用日语说出来。</div>
      <div class="rec-zh-hintbox">${recZhHintHTML(l)}</div>
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-zh-hint" ${lvl >= 4 ? 'disabled' : ''}>${lvl >= 4 ? '提示已用完' : `提示 ${lvl + 1}/4`}</button>
        <button class="btn ghost" id="rec-zh-show">${lvl >= 4 ? '▶ 听原声' : '看答案'}</button>
        <button class="btn accent wide" id="rec-zh-next">下一句 →</button>
      </div>
      <div class="rec-drill-progress"><span>零提示 ${REC_ZH_CLEAN} 句</span><span>用过提示 ${REC_ZH_USED} 句</span></div>
    </div>`;
  $('rec-zh-hint').onclick = recZhHint;
  $('rec-zh-show').onclick = recZhShow;
  $('rec-zh-next').onclick = recZhNext;
}
function recZhHint() {
  if (!REC || REC_MODE !== 'zh2jp' || REC_ZH_LVL >= 4) return;
  REC_ZH_LVL++;
  REC_ZH_USED_THIS = true;
  renderZh2Jp();
}
function recZhShow() {
  if (!REC || REC_MODE !== 'zh2jp') return;
  if (REC_ZH_LVL < 4) {
    REC_ZH_LVL = 4;
    REC_ZH_USED_THIS = true;
    renderZh2Jp();
    return;
  }
  recPlayAt(REC_IDX, false);
}
function recZhNext() {
  if (!REC || REC_MODE !== 'zh2jp') return;
  const l = recLine();
  if (!l) return;
  if (REC_ZH_USED_THIS) REC_ZH_USED++; else REC_ZH_CLEAN++;
  if (!l.recited) {
    l.recited = true;
    recPost(l.line_no, 'recite', '');
  }
  if (REC_IDX >= REC.lines.length - 1) { renderZh2JpDone(); return; }
  REC_IDX++;
  recSavePos();
  REC_ZH_LVL = 0;
  REC_ZH_USED_THIS = false;
  renderZh2Jp();
}
function renderZh2JpDone() {
  if (!REC) return;
  const total = REC.lines.length;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-role-done">
      <div class="rec-focus-top"><span>中译日</span><span>完成</span></div>
      <div class="rec-role-curtain">说完了</div>
      <div class="rec-role-stats">
        <span>完成 <b>${total}</b> 句</span>
        <span>零提示 <b>${REC_ZH_CLEAN}</b> 句</span>
        <span>用过提示 <b>${REC_ZH_USED}</b> 句</span>
      </div>
      ${REC_ZH_USED === 0 ? '<div class="rec-role-perfect">全程零提示，从中文直接说出日语。</div>' : ''}
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-zh-again">再来一遍</button>
        <button class="btn accent wide" id="rec-zh-back">返回列表</button>
      </div>
    </div>`;
  $('rec-zh-again').onclick = () => {
    REC_IDX = 0;
    recSavePos();
    recZhBegin();
  };
  $('rec-zh-back').onclick = () => goBack();
}

// ---- 通关赛 ----
// 一课一局：零提示背完加连击；看过答案的句子结尾进最多 3 句加时赛。
function recRunSavedBest() {
  return REC ? (parseInt(localStorage.getItem('ts_rec_run_best_' + REC.lesson.id) || '0', 10) || 0) : 0;
}
function recRunBegin() {
  REC_RUN_PREV_BEST = recRunSavedBest();
  REC_RUN_COMBO = 0;
  REC_RUN_BEST = 0;
  REC_RUN_CLEAN = 0;
  REC_RUN_USED = 0;
  REC_RUN_FLAGGED = [];
  REC_RUN_OT = [];
  REC_RUN_OT_POS = 0;
  REC_RUN_OT_HINTS = 0;
  REC_RUN_PHASE = 'main';
  REC_RUN_REVEAL = false;
  REC_RUN_SEEN = false;
  REC_IDX = 0;
  recSavePos();
  renderRunLine();
  recPlayAt(REC_IDX, false, true);
}
function recRunBumpCombo() {
  REC_RUN_COMBO++;
  if (REC_RUN_COMBO > REC_RUN_BEST) REC_RUN_BEST = REC_RUN_COMBO;
  const key = 'ts_rec_run_best_' + REC.lesson.id;
  if (REC_RUN_BEST > (parseInt(localStorage.getItem(key) || '0', 10) || 0)) {
    localStorage.setItem(key, String(REC_RUN_BEST));
  }
}
function renderRunLine() {
  const l = recLine();
  if (!l) return;
  const ot = REC_RUN_PHASE === 'overtime';
  const pos = ot ? REC_RUN_OT_POS : REC_IDX;
  const total = ot ? REC_RUN_OT.length : REC.lines.length;
  const jp = REC_RUN_REVEAL ? recJP(l) : recHidden(l);
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-run">
      <div class="rec-focus-top">
        <span>第 ${pos + 1} / ${total} 句 · ${ot ? '加时赛' : '通关赛'}</span>
        <span class="rec-run-combo${REC_RUN_COMBO ? ' on' : ''}">${ot ? '回炉重背' : '连击 ×' + REC_RUN_COMBO}</span>
      </div>
      ${ot ? '<div class="rec-role-scene">这一句刚才看过答案，再来一次。</div>' : ''}
      <div class="rec-focus-jp" id="rec-run-jp">${jp}</div>
      <div class="rec-focus-zh${REC_RUN_REVEAL ? '' : ' hide'}" id="rec-run-zh">${esc(l.translation)}</div>
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-run-play">▶ 播放本句</button>
        <button class="btn ghost" id="rec-run-show">${REC_RUN_REVEAL ? '收起答案' : '看答案'}</button>
        <button class="btn accent wide" id="rec-run-done">${ot ? '过关 →' : '会背了'}</button>
      </div>
      <div class="rec-drill-progress">
        ${ot
          ? `<span>加时赛 ${REC_RUN_OT_POS + 1} / ${REC_RUN_OT.length} 句</span><span>又看答案 ${REC_RUN_OT_HINTS} 句</span>`
          : `<span>零提示 ${REC_RUN_CLEAN} 句 · 看答案 ${REC_RUN_USED} 句</span><span>最高连击 ${REC_RUN_BEST}</span>`}
      </div>
    </div>`;
  $('rec-run-play').onclick = () => recPlayAt(REC_IDX, false);
  $('rec-run-show').onclick = recRunToggleAnswer;
  $('rec-run-done').onclick = ot ? recRunOtDone : recRunPass;
}
function recRunToggleAnswer() {
  if (!REC || REC_MODE !== 'run') return;
  if (REC_RUN_REVEAL) {
    REC_RUN_REVEAL = false;
  } else {
    REC_RUN_REVEAL = true;
    REC_RUN_SEEN = true;
    if (REC_RUN_PHASE === 'main' && REC_RUN_COMBO) REC_RUN_COMBO = 0;
  }
  renderRunLine();
}
function recRunPass() {
  if (!REC || REC_MODE !== 'run' || REC_RUN_PHASE !== 'main') return;
  const l = recLine();
  if (!l) return;
  if (REC_RUN_SEEN) {
    REC_RUN_USED++;
    if (!REC_RUN_FLAGGED.includes(REC_IDX)) REC_RUN_FLAGGED.push(REC_IDX);
  } else {
    REC_RUN_CLEAN++;
    recRunBumpCombo();
  }
  if (!l.recited) {
    l.recited = true;
    recPost(l.line_no, 'recite', '');
  }
  if (REC_IDX >= REC.lines.length - 1) { recRunFinishMain(); return; }
  REC_IDX++;
  recSavePos();
  REC_RUN_REVEAL = false;
  REC_RUN_SEEN = false;
  renderRunLine();
  recPlayAt(REC_IDX, false, true);
}
function recRunFinishMain() {
  if (!REC) return;
  if (REC_RUN_FLAGGED.length) {
    REC_RUN_OT = REC_RUN_FLAGGED.slice(0, 3);
    REC_RUN_OT_POS = 0;
    REC_RUN_OT_HINTS = 0;
    REC_RUN_PHASE = 'overtime';
    REC_RUN_REVEAL = false;
    REC_RUN_SEEN = false;
    REC_IDX = REC_RUN_OT[0];
    recSavePos();
    renderRunLine();
    recPlayAt(REC_IDX, false, true);
    return;
  }
  renderRunDone();
}
function recRunOtDone() {
  if (!REC || REC_MODE !== 'run' || REC_RUN_PHASE !== 'overtime') return;
  if (REC_RUN_SEEN) REC_RUN_OT_HINTS++;
  if (REC_RUN_OT_POS >= REC_RUN_OT.length - 1) { renderRunDone(); return; }
  REC_RUN_OT_POS++;
  REC_IDX = REC_RUN_OT[REC_RUN_OT_POS];
  recSavePos();
  REC_RUN_REVEAL = false;
  REC_RUN_SEEN = false;
  renderRunLine();
  recPlayAt(REC_IDX, false, true);
}
function renderRunDone() {
  if (!REC) return;
  REC_RUN_PHASE = 'done';
  const total = REC.lines.length;
  const record = REC_RUN_BEST > REC_RUN_PREV_BEST;
  $('rec-focus').innerHTML = `
    <div class="rec-focus rec-role-done">
      <div class="rec-focus-top"><span>通关赛</span><span>完成</span></div>
      <div class="rec-role-curtain">本课通关</div>
      <div class="rec-role-stats">
        <span>最高连击 <b>${REC_RUN_BEST}</b>${record ? ' · 新纪录' : ''}</span>
        <span>零提示 <b>${REC_RUN_CLEAN} / ${total}</b> 句 · 看答案 <b>${REC_RUN_USED}</b> 句</span>
        ${REC_RUN_OT.length ? `<span>加时赛 <b>${REC_RUN_OT.length}</b> 句${REC_RUN_OT_HINTS ? ` · 又看答案 ${REC_RUN_OT_HINTS} 句` : ''}</span>` : ''}
      </div>
      ${REC_RUN_USED === 0 ? '<div class="rec-role-perfect">零提示通关，一课一次过。</div>' : ''}
      <div class="rec-focus-actions">
        <button class="btn ghost" id="rec-run-again">再来一局</button>
        <button class="btn accent wide" id="rec-run-back">返回列表</button>
      </div>
    </div>`;
  $('rec-run-again').onclick = recRunBegin;
  $('rec-run-back').onclick = () => goBack();
}

// ============ 本课/本单元播客（v13：思思+xudong 单场双语） ============
let POD = null;      // {id, timeline, duration_ms, audio, url, cur}
let POD_SRC = null;  // {kind:'lesson'|'dir', id}
let POD_HOME = null; // 播放器面板的默认父节点（学习页）
function podMount(slot) {
  const p = $('pod-panel');
  if (!POD_HOME) POD_HOME = p.parentNode;
  const target = slot || POD_HOME;
  if (p.parentNode !== target) target.appendChild(p);
}
function podSrcPath() { return POD_SRC.kind === 'dir' ? '/podcast/by-dir/' : '/podcast/by-lesson/'; }
function podFmt(ms) {
  const s = Math.floor(ms / 1000), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), ss = s % 60;
  return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(ss).padStart(2, '0'); // 长片要能显示到小时
}
const podSleep = (ms) => new Promise(r => setTimeout(r, ms));
let POD_T0 = 0;      // 本次生成的起点（日志时间戳用）
let POD_LOG_CUR = null; // 正在原地刷新的那行日志
function podLogInit() { const l = $('pod-log'); l.hidden = false; l.innerHTML = ''; POD_LOG_CUR = null; }
// podLog：replace=true 时原地刷新当前行（轮次/秒数在跳），否则追加新行（阶段变了）
function podLog(msg, replace) {
  const l = $('pod-log');
  const txt = podFmt(Date.now() - POD_T0) + '  ' + msg;
  if (replace && POD_LOG_CUR) { POD_LOG_CUR.textContent = txt; l.scrollTop = l.scrollHeight; return; }
  if (POD_LOG_CUR) POD_LOG_CUR.classList.remove('cur');
  const d = document.createElement('div');
  d.className = 'cur';
  d.textContent = txt;
  l.appendChild(d);
  POD_LOG_CUR = d;
  l.scrollTop = l.scrollHeight; // 只滚日志框，页面不动
}
async function podOpen(kind, id, slot) {
  if (!id) { return; }
  POD_SRC = { kind, id };
  POD_EST = null;
  podMount(slot);
  $('pod-panel').hidden = false;
  $('pod-log').hidden = true;
  $('pod-title').textContent = (kind === 'dir' ? '本单元播客' : '本课播客') + ' · 思思 & xudong';
  $('pod-state').textContent = '载入中…';
  podEstimate(); // 不 await：只影响按钮文案与提示
  // 服务端可能还有一集在跑（关页面/锁屏不会中断它）→ 直接接着看进度
  const q = kind === 'dir' ? 'dir_id=' + id : 'lesson_id=' + id;
  let job = null;
  try { job = await api('/podcast/jobs/latest?' + q); } catch (e) { /* 没有任务，正常 */ }
  if (job && ['queued', 'writing', 'synthesizing'].includes(job.status)) {
    podReset();
    POD_T0 = Date.now();
    podLogInit();
    podLog('接着上次的进度（任务在服务端跑，关页面也不会断）');
    await podWatch(job.id);
    return;
  }
  try {
    const meta = await api(podSrcPath() + id);
    await podLoad(meta);
  } catch (e) {
    podReset();
  }
}
// podEstimate 生成前的时长/成本预估：长片一集十几分钟、按 token 计费，先让你知道要花多少
let POD_EST = null;
async function podEstimate() {
  if (!POD_SRC) return null;
  const q = POD_SRC.kind === 'dir' ? 'dir_id=' + POD_SRC.id : 'lesson_id=' + POD_SRC.id;
  try {
    const e = await api('/podcast/estimate?' + q);
    POD_EST = e;
    const btn = $('pod-gen');
    btn.textContent = '生成播客（约 ' + e.est_minutes + ' 分钟）';
    btn.title = `${e.words} 词 · ${e.segs} 段写稿 · 预计 ${e.est_minutes} 分钟音频 · 约 ${e.est_yuan} 元`;
    podShowEstimate();
    return e;
  } catch (e) { return null; /* 估算失败不影响生成 */ }
}
function podReset() {
  podStopAudio();
  $('pod-state').textContent = '还没有播客';
  $('pod-list').innerHTML = '';
  $('pod-sub').textContent = '';
  $('pod-time').textContent = '0:00 / 0:00';
  $('pod-gen').hidden = false;
  podShowEstimate(); // 估算可能比 podReset 先回来，这里补一次
}
// podShowEstimate 只在"还没有播客"的空白态下补预估，不覆盖已生成/生成中的状态
function podShowEstimate() {
  if (!POD_EST || !$('pod-state').textContent.startsWith('还没有播客')) return;
  $('pod-state').textContent = `还没有播客 · ${POD_EST.words} 词 · 约 ${POD_EST.est_minutes} 分钟 · 约 ${POD_EST.est_yuan} 元`;
}
async function podLoad(meta) {
  podStopAudio();
  POD = { id: meta.episode_id, timeline: meta.timeline, duration_ms: meta.duration_ms, cur: -1, follow: true, autoScrolling: false };
  $('pod-gen').hidden = true;
  $('pod-state').textContent = '已生成 · ' + meta.rounds + ' 轮 · ' + podFmt(meta.duration_ms);
  podRenderList(); // 列表与时间轴先出来：长集音频 15—25MB，不能等整包下载（手机上打不开，2026-11-04 实测）
  podTick();
  const pj = await api('/podcast/' + meta.episode_id + '/play');
  const a = new Audio();
  a.preload = 'metadata';
  a.src = pj.url; // 浏览器原生流式 + Range：秒开、能拖进度、不用把整集读进内存
  POD.audio = a;
  a.playbackRate = parseFloat(localStorage.getItem('ts_pod_rate') || '1');
  $('pod-rate').value = String(a.playbackRate);
  a.addEventListener('timeupdate', podTick);
  a.addEventListener('ended', () => { $('pod-play').textContent = '▶'; });
  a.addEventListener('loadedmetadata', () => {
    const pos = parseFloat(localStorage.getItem('ts_pod_pos_' + POD.id) || '0'); // 记住上次位置（毫秒）
    if (pos > 1000 && pos < meta.duration_ms - 3000) a.currentTime = pos / 1000;
  });
  let retried = false;
  a.addEventListener('error', async () => {
    if (!POD || POD.audio !== a) return;
    if (retried) { $('pod-state').textContent = '音频加载失败，请重试'; return; }
    retried = true; // 票据 30 分钟过期或网络抖一下：换一张票据从原位置续
    const at = a.currentTime;
    try {
      const np = await api('/podcast/' + meta.episode_id + '/play');
      a.src = np.url;
      a.currentTime = at;
    } catch (e) {
      $('pod-state').textContent = '音频加载失败，请重试';
    }
  });
}
function podRenderList() {
  const box = $('pod-list'); box.innerHTML = '';
  POD.timeline.forEach((s) => {
    const row = document.createElement('div');
    row.className = 'pod-row' + (HasJa(s.text) ? ' ja' : '');
    row.innerHTML = `<span class="who ${s.role}">${s.role === 'a' ? '思' : 'xu'}</span><span class="tx"></span>`;
    row.querySelector('.tx').textContent = s.text;
    row.onclick = () => { POD.follow = true; POD.audio.currentTime = s.start_ms / 1000; podPlay(true); };
    box.appendChild(row);
  });
}
function HasJa(s) { return /[\u3040-\u30FF\uFF66-\uFF9F]/.test(s); }
function podTick() {
  if (!POD || !POD.audio) return;
  const t = POD.audio.currentTime * 1000;
  $('pod-time').textContent = podFmt(t) + ' / ' + podFmt(POD.duration_ms);
  let idx = -1;
  for (let i = 0; i < POD.timeline.length; i++) {
    if (t >= POD.timeline[i].start_ms && t < POD.timeline[i].end_ms) { idx = i; break; }
  }
  if (idx !== POD.cur) {
    POD.cur = idx;
    const rows = $('pod-list').children;
    for (let i = 0; i < rows.length; i++) rows[i].classList.toggle('on', i === idx);
    $('pod-sub').textContent = idx >= 0 ? POD.timeline[idx].text : '';
    if (idx >= 0 && POD.follow) podScrollToRow(rows[idx]);
  }
  if (POD.audio.currentTime > 0.5) localStorage.setItem('ts_pod_pos_' + POD.id, String(Math.floor(t))); // 记住上次位置（加载瞬间的 0 不覆写）
}
// 跟随时把当前行平滑滚到列表中央；用户手动滚过（POD.follow=false）就完全不动，
// 与连播列表同一套静默期逻辑：程序滚动期间的 scroll 事件不误判为用户滚动
let podAutoTimer = 0, podAutoDeadline = 0;
function podBeginAutoScroll() {
  POD.autoScrolling = true;
  podAutoDeadline = Date.now() + 2500;
  clearTimeout(podAutoTimer);
  podAutoTimer = setTimeout(() => { if (POD) POD.autoScrolling = false; }, 200);
}
function podUserScrolled() {
  if (!POD) return;
  clearTimeout(podAutoTimer);
  POD.autoScrolling = false;
  POD.follow = false;
}
function podScrollToRow(row) {
  const list = $('pod-list');
  const rt = row.offsetTop, lh = row.offsetHeight, vt = list.scrollTop, vh = list.clientHeight;
  if (rt < vt + 4 || rt + lh > vt + vh - 4) {
    podBeginAutoScroll();
    list.scrollTo({ top: Math.max(0, rt - vh / 2 + lh / 2), behavior: 'smooth' });
  }
}
function podPlay(on) {
  if (!POD || !POD.audio) return;
  if (on) { POD.follow = true; POD.audio.play(); $('pod-play').textContent = '⏸'; }
  else { POD.audio.pause(); $('pod-play').textContent = '▶'; }
}
function podStopAudio() {
  if (POD && POD.audio) { try { POD.audio.pause(); POD.audio.src = ''; } catch (e) {} } // 断开流式下载
  POD = null;
}
async function podGenerate() {
  if (!POD_SRC) return;
  const est = POD_EST || await podEstimate();
  if (est && !confirm(`${est.title}：${est.words} 个词\n预计 ${est.est_minutes} 分钟音频，合成成本约 ${est.est_yuan} 元；生成要跑十几分钟。\n\n生成期间可以关掉页面，回来接着看进度。\n\n确定开始？`)) return;
  $('pod-gen').disabled = true;
  POD_T0 = Date.now();
  podLogInit();
  try {
    const body = POD_SRC.kind === 'dir' ? { dir_id: POD_SRC.id } : { lesson_id: POD_SRC.id };
    const r = await fetchAuth('/api/v1/podcast/jobs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) {
      const j = await r.json().catch(() => ({}));
      throw new Error((j.error && j.error.message) || ('HTTP ' + r.status));
    }
    const job = await r.json();
    podLog(`已排队：${job.words} 词，预计 ${job.est_minutes} 分钟音频。可以关掉页面，回来接着看。`);
    await podWatch(job.id);
  } catch (e) {
    podLog('失败：' + e.message);
    $('pod-state').textContent = '生成失败：' + e.message;
    $('pod-gen').disabled = false;
  }
}

// podWatch 轮询后台任务：阶段变化时追加一行，同阶段原地刷新数字（不刷屏）。
// 任务在服务端跑，轮询断了也不影响生成——重新打开面板会接着显示进度。
async function podWatch(jobId) {
  let lastKey = '';
  for (;;) {
    let j;
    try { j = await api('/podcast/jobs/' + jobId); } catch (e) { await podSleep(3000); continue; }
    const key = j.status + '|' + (j.status === 'writing' ? j.seg : '');
    const detail = j.status === 'writing' && j.segs ? ` ${j.seg}/${j.segs} 段`
      : (j.status === 'synthesizing' && j.rounds ? ` ${j.round}/${j.rounds} 轮` : '');
    podLog((j.stage || '处理中…') + detail, key === lastKey);
    lastKey = key;
    $('pod-state').textContent = '生成中 ' + podFmt(Date.now() - POD_T0) + ' · ' + (j.stage || '');
    if (j.status === 'error') {
      podLog('失败：' + (j.error || '未知错误'));
      $('pod-state').textContent = '生成失败';
      $('pod-gen').disabled = false;
      return;
    }
    if (j.status === 'done') break;
    await podSleep(2000);
  }
  podLog('完成，开始播放');
  $('pod-state').textContent = '载入音频…';
  try {
    const meta = await api(podSrcPath() + POD_SRC.id);
    await podSleep(400);
    $('pod-log').hidden = true;
    await podLoad(meta);
    podPlay(true);
  } catch (e) {
    podLog('音频载入失败：' + e.message);
    $('pod-state').textContent = '生成完成，但音频载入失败：' + e.message;
  }
  $('pod-gen').disabled = false;
}

// ============ 启动 ============
// 原内联 onclick/oninput 全部改为绑定（CSP 禁内联脚本）
$('nav-back').onclick = goBack;
$('nav-home').onclick = goHome;
$('logout-btn').onclick = doLogout;
$('login-btn').onclick = doLogin;
$('home-main-btn').onclick = () => go('create');
$('home-new-btn').onclick = () => go('create');
$('review-btn').onclick = goReview;
$('ws-due-btn').onclick = () => goWordStudy('due');
$('ws-new-btn').onclick = () => goWordStudy('new');
$('lib-link').onclick = (e) => { e.preventDefault(); goLibrary(); };
$('home-nb-btn').onclick = goNotebook;
$('home-rec-btn').onclick = goRecite;
$('cache-clear-btn').onclick = clearAudioCache;
$('mode-full').onclick = () => pickGenMode('full');
$('mode-phrases').onclick = () => pickGenMode('phrases');
$('submit-words-btn').onclick = submitWords;
$('open-picker-btn').onclick = openWordPicker;
$('fill-sample-btn').onclick = fillSample;
$('gen-retry').onclick = submitWords;
$('gen-back').onclick = goBack;
$('md-normal').onclick = () => { switchView('material'); setMode('normal'); };
$('md-blind').onclick = () => setMode('blind');
$('md-zh').onclick = () => setMode('zh');
$('md-rt').onclick = toggleRT;
$('md-fav').onclick = () => switchView('fav');
$('vw-words').onclick = () => switchView('words');
$('vw-phrases').onclick = () => switchView('phrases');
$('vw-full').onclick = () => switchView('material');
$('play-all-btn').onclick = playAll;
$('tts-btn').onclick = genAllAudio;
$('lib-search').oninput = e => libSearch(e.target.value);
$('picker-search').oninput = e => pickerSearch(e.target.value);
$('picker-ok').onclick = pickerConfirm;
$('picker-cancel-btn').onclick = closePicker;
$('ai-send').onclick = aiSend;
$('nb-strip').onclick = openNbPicker;
$('nb-play-all').onclick = (e) => { e.stopPropagation(); nbPlayStart(); };
$('nb-kbd-btn').onclick = (e) => {
  e.stopPropagation(); // 横条点击会开选择面板，需阻断
  $('nb-kbd-btn').classList.toggle('tip-open');
};
document.addEventListener('click', e => {
  if (!e.target.closest || !e.target.closest('#nb-kbd-btn')) $('nb-kbd-btn').classList.remove('tip-open');
});
$('nb-play-cfg').onclick = (e) => { e.stopPropagation(); openNbCfg(); };
$('nb-copy-btn').onclick = (e) => { e.stopPropagation(); nbCopyAll(); };
$('nb-export-btn').onclick = (e) => { e.stopPropagation(); nbExportAll(); };
$('nb-test-btn').onclick = (e) => { e.stopPropagation(); nbStartTest(); };

// ---- 课文本 ----
document.querySelectorAll('#rec-modes button').forEach(b => {
  b.onclick = () => setRecMode(b.dataset.mode);
});
$('rec-play').onclick = recToggle;
$('rec-prev').onclick = recPrev;
$('rec-next').onclick = recNext;
$('rec-repeat').onclick = () => { if (REC) recPlayAt(REC_IDX, REC_MODE === 'listen'); };
$('rec-rate').value = String(REC_RATE);
$('rec-rate').onchange = e => {
  REC_RATE = parseFloat(e.target.value) || 1;
  localStorage.setItem('ts_rec_rate', String(REC_RATE));
  if (REC_AUDIO) REC_AUDIO.playbackRate = REC_RATE;
};
$('rec-list').addEventListener('click', e => {
  const el = e.target.closest('.rec-item');
  if (el) openRec(el.dataset.id);
});
$('rec-lines').addEventListener('click', e => {
  const el = e.target.closest('.rec-line');
  if (!el || !REC) return;
  recPlayAt(+el.dataset.idx, REC_MODE === 'listen');
});
addEventListener('wheel', () => { if (CUR_SCREEN === 'recite-play') REC_FOLLOW = false; }, { passive: true });
addEventListener('touchmove', () => { if (CUR_SCREEN === 'recite-play') REC_FOLLOW = false; }, { passive: true });

// ---- 线上 nginx 的 CSP 是 script-src 'self'：内联 onclick/ontoggle 一律被拦（2026-11-06 实锤），
// 渲染模板只写 data-act/data-arg，这里统一委托绑定 ----
document.addEventListener('click', (e) => {
  const el = e.target.closest ? e.target.closest('[data-act]') : null;
  if (!el) return;
  const arg = el.dataset.arg || '';
  switch (el.dataset.act) {
    case 'gen-phrases': genPhrases(); break;
    case 'gen-full': genFull(); break;
    case 'rv-play': reviewPlayAudio(); break;
    case 'rv-reveal': reviewReveal(); break;
    case 'rv-fail': reviewResult('fail'); break;
    case 'rv-pass': reviewResult('pass'); break;
    case 'rv-master': reviewMaster(); break;
    case 'home': goHome(); break;
    case 'ws-seg': wsSetMode(arg); break;
    case 'ws-sent': wsPlayQuizSent(el); break;
    case 'ws-speak': wsSpeakWord(arg, el); break;
    case 'ws-type': wsTypeConfirm(); break;
    case 'ws-flip': wsFlip(); break;
    case 'ws-fail': wsResult('fail'); break;
    case 'ws-pass': wsResult('pass'); break;
    case 'ws-next': wsNext(); break;
    case 'ws-quit': wsQuit(); break;
  }
});
// toggle 事件不冒泡：捕获阶段记"用过语境提示"
document.addEventListener('toggle', (e) => {
  const d = e.target;
  if (d && d.classList && d.classList.contains('ws-hint') && d.open) WS_HINT_USED = true;
}, true);
$('nb-now-stop').onclick = () => nbPlayStop();
$('nb-now-pause').onclick = nbPlayPause;
// 全播中 ←/↑ 上一个、→/↓ 下一个、空格暂停/继续（只在生词库列表页、非输入框里生效）
document.addEventListener('keydown', e => {
  if (!PL || CUR_SCREEN !== 'notebook') return;
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  if (['picker-mask', 'nb-cfg-mask'].some(id => $(id) && getComputedStyle($(id)).display !== 'none')) return;
  const t = e.target;
  if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || ''))) return;
  if (e.key === ' ' || e.key === 'Spacebar') {
    if (e.repeat) return;
    if (t && t.closest && t.closest('button, a, [role="button"]')) return; // 焦点在按钮上时留给原生空格激活
    nbPlayPause();
    e.preventDefault();
    return;
  }
  const step = (e.key === 'ArrowRight' || e.key === 'ArrowDown') ? 1
    : (e.key === 'ArrowLeft' || e.key === 'ArrowUp') ? -1 : 0;
  if (!step) return;
  if (nbPlaySkip(step)) e.preventDefault();
});
$('pod-btn').onclick = () => podOpen('lesson', LESSON && LESSON.lesson && LESSON.lesson.id, null);
$('nb-pod-btn').onclick = (e) => {
  e.stopPropagation(); // 横条点击会开选择面板，需阻断
  if (!NB_CURDIR) { toast('先选择目录'); return; }
  podOpen('dir', NB_CURDIR, $('pod-slot-nb'));
};
$('pod-close').onclick = () => { podStopAudio(); $('pod-panel').hidden = true; };
$('pod-gen').onclick = podGenerate;
$('pod-play').onclick = () => { if (POD) podPlay(POD.audio.paused); };
$('pod-rate').onchange = e => {
  const v = parseFloat(e.target.value);
  localStorage.setItem('ts_pod_rate', String(v));
  if (POD && POD.audio) POD.audio.playbackRate = v;
};
// 用户主动滚动/触摸滑动后停止自动跟随；程序滚动期间（autoScrolling）续期静默窗口，不误判
addEventListener('wheel', () => { userScrolled(); podUserScrolled(); }, { passive: true });
addEventListener('touchmove', () => { userScrolled(); podUserScrolled(); }, { passive: true });
addEventListener('scroll', () => {
  if (!PL) return;
  if (PL.autoScrolling) {
    if (Date.now() < autoScrollDeadline) { armAutoScrollEnd(); return; }
    PL.autoScrolling = false;
  }
  PL.follow = false;
}, { passive: true });
// 播客字幕列表是内层滚动容器（scroll 不冒泡到 window），单独挂
$('pod-list').addEventListener('scroll', () => {
  if (!POD) return;
  if (POD.autoScrolling) {
    if (Date.now() < podAutoDeadline) { podBeginAutoScroll(); return; }
    POD.autoScrolling = false;
  }
  POD.follow = false;
}, { passive: true });
$('cfg-save').onclick = saveNbCfg;
$('cfg-cancel').onclick = () => { $('nb-cfg-mask').style.display = 'none'; };
$('nb-cfg-mask').addEventListener('click', e => {
  if (e.target === $('nb-cfg-mask')) $('nb-cfg-mask').style.display = 'none';
});
$('ai-q').addEventListener('keydown', e => { if (e.key === 'Enter') aiSend(); });
if (TOKEN) afterLogin(); else { $('userchip').textContent = '未登录'; go('login'); }