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
// 页面树：home 为根；create/learn/review/library/wordstudy/notebook 为一级；
// gen 是中转页（生成中），其上一级 = 进入它时的来源页（create 或 learn）。
const NAV_PARENT = {
  home: null, login: null,
  create: 'home', learn: 'home', review: 'home',
  library: 'home', wordstudy: 'home', notebook: 'home',
};
let CUR_SCREEN = 'login';
let GEN_FROM = 'create';

function go(name) {
  if (name === 'gen') GEN_FROM = CUR_SCREEN; // 记录中转页来源
  CUR_SCREEN = name;
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('on'));
  $('sc-' + name).classList.add('on');
  updateBackBtn();
}

// goBack 返回上一级；根页面不响应
function goBack() {
  if (CUR_SCREEN === 'home' || CUR_SCREEN === 'login') return;
  const target = CUR_SCREEN === 'gen' ? GEN_FROM : (NAV_PARENT[CUR_SCREEN] || 'home');
  go(target);
}

function updateBackBtn() {
  const b = $('nav-back');
  if (b) b.style.display = (CUR_SCREEN === 'home' || CUR_SCREEN === 'login') ? 'none' : 'inline-block';
}
function showErr(id, msg) { const e = $(id); e.textContent = msg; e.style.display = 'block'; }
function hideErr(id) { $(id).style.display = 'none'; }
function esc(s) { return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

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
  $('logout-btn').style.display = 'block';
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
  if (existing) { existing.remove(); return; } // 再次点击收起
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
        <button class="btn accent" onclick="genPhrases()">为 ${LESSON.words.length} 个生词生成短语</button>
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
    $('gen-back').textContent = '返回学习';
    $('gen-back').onclick = () => { go('learn'); };
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
    $('gen-back').onclick = () => { go('learn'); };
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
    $('gen-back').onclick = () => { clearActiveTask(); goHome(); };
  }
}

// loadHome 回到首页枢纽：今日状态 + 历史课程入口（学习屏的返回落点）
async function loadHome() {
  if (!TOKEN) { go('login'); return; } // 未登录防御：绝不停留在需鉴权的屏
  const today = new Date().toISOString().slice(0, 10);
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
    $('lesson-date').textContent = todayIt.date;
    $('lesson-desc').textContent = `今日课程已就绪（${todayIt.word_count} 词）`;
    btn.textContent = '▶ 继续学习';
    btn.onclick = openLesson(todayIt.lesson_id);
    // 今日已生成时保留「新建词单」入口（重做今日：确认后删课重建）
    $('home-new-btn').style.display = 'block';
  } else if (todayIt) {
    chip.textContent = '准备中'; chip.classList.remove('ready');
    $('lesson-date').textContent = todayIt.date;
    $('lesson-desc').textContent = '今日课程草稿就绪，可继续提交词单生成。';
    btn.textContent = '继续提交词单';
    btn.onclick = () => go('create');
    $('home-new-btn').style.display = 'none';
  } else {
    chip.textContent = '未创建'; chip.classList.remove('ready');
    $('lesson-date').textContent = '—';
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
function rubyHTML(tokens) {
  if (!Array.isArray(tokens) || !tokens.length) return '';
  return tokens
    .map(([w, r]) => (r && /[一-龯]/.test(w)) ? `<ruby>${esc(w)}<rt>${esc(r)}</rt></ruby>` : esc(w))
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
        <button class="btn accent" onclick="genFull()">生成对话与短文</button>
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
  if (hit && hit.blob) { idbPut(audioId, hit.blob); return URL.createObjectURL(hit.blob); }
  const r = await fetch('/api/v1/audio/' + audioId + '/file', { headers: { Authorization: 'Bearer ' + TOKEN } });
  if (!r.ok) throw new Error('音频获取失败 HTTP ' + r.status);
  const blob = await r.blob();
  idbPut(audioId, blob);
  return URL.createObjectURL(blob);
}
// 缓存优先取单词发音 blob URL（key 加 w: 前缀与句子音频区分）
async function wordAudioBlobURL(wordId) {
  const key = 'w:' + wordId;
  const hit = await idbGet(key);
  if (hit && hit.blob) { idbPut(key, hit.blob); return URL.createObjectURL(hit.blob); }
  const r = await fetch('/api/v1/word-study/' + wordId + '/audio', { headers: { Authorization: 'Bearer ' + TOKEN } });
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
      <button class="rv-play" id="rv-play-btn" onclick="reviewPlayAudio()">${hasAudio ? '▶' : '…'}</button>
      <div id="rv-detail" style="display:none">
        <div class="rv-text">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
        <div class="rv-zh">${esc(s.translation || '')}</div>
        <div class="rv-meta">${srcLabel[it.source] || it.source} · 间隔 ${it.interval_days} 天 · 错 ${it.error_count} 次</div>
      </div>
      <div class="rv-actions" id="rv-reveal-row" style="display:none">
        <button class="btn ghost" onclick="reviewReveal()">显示原文</button>
      </div>
      <div class="rv-actions" id="rv-result-row" style="display:none">
        <button class="btn" onclick="reviewResult('fail')">不认识</button>
        <button class="btn accent" onclick="reviewResult('pass')">认识</button>
      </div>
      <div style="text-align:center;margin-top:12px">
        <button class="hint" style="background:none;border:none;cursor:pointer;color:var(--ink2);font-size:12px"
          onclick="reviewMaster()">已掌握，移出复习</button>
      </div>
    </div>
    <button class="btn ghost" onclick="goHome()" style="margin-top:8px">返回首页</button>`;
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
      <button class="btn accent" onclick="goHome()">返回首页</button>
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

async function goWordStudy(mode) {
  WS_MODE = mode;
  try {
    const r = await api('/word-study/' + (mode === 'new' ? 'new' : 'due'));
    WS_ITEMS = r.words || [];
  } catch (e) { alert(e.message); return; }
  if (!WS_ITEMS.length) { alert(mode === 'new' ? '没有待学的新词了。' : '今日没有到期的复习词。'); return; }
  WS_IDX = 0; WS_PASS = 0; WS_FAIL = 0;
  go('wordstudy');
  renderWordStudyItem();
}

function wsStopAudio() { if (WS_AUDIO) { WS_AUDIO.pause(); WS_AUDIO = null; } }

// wsSpeakWord 单词发音：后端 /word-study/{id}/audio（首次合成落库，之后直接用缓存）
function wsSpeakWord(wordId, btn) {
  return new Promise(async resolve => {
    try {
      if (btn) { btn.textContent = '⏳'; }
      const url = await wordAudioBlobURL(wordId);
      wsStopAudio();
      WS_AUDIO = new Audio(url);
      WS_AUDIO.onended = WS_AUDIO.onerror = () => { if (btn) btn.textContent = '🔊'; resolve(); };
      await WS_AUDIO.play();
    } catch (e) {
      if (btn) btn.textContent = '🔊';
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

function renderWordStudyItem() {
  wsStopAudio();
  const box = $('wordstudy-content');
  if (WS_IDX >= WS_ITEMS.length) { renderWordStudyDone(); return; }
  const w = WS_ITEMS[WS_IDX];
  const total = WS_ITEMS.length;
  box.innerHTML = `
    <div class="card">
      <div class="rv-progress">
        <div class="bar"><i style="width:${(WS_IDX / total * 100)}%"></i></div>
        <span>${WS_IDX + 1} / ${total}</span>
      </div>
      <div class="ws-front" id="ws-front">
        <div class="ws-word-line">
          <span class="ws-word-txt">${esc(w.surface)}</span>
          <button class="ws-mini" onclick="wsSpeakWord('${w.id}', this)" title="听单词发音">🔊</button>
        </div>
        <p class="hint">回想它的读音和意思，然后翻面</p>
        <button class="btn ghost" onclick="wsFlip()">翻面</button>
      </div>
      <div class="ws-back" id="ws-back" style="display:none">
        <div class="ws-word-line">
          <ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby>
          <button class="ws-mini" onclick="wsSpeakWord('${w.id}', this)" title="听单词发音">🔊</button>
        </div>
        <div class="ws-meaning">${esc(w.meaning || '')} <span class="lib-meta">${esc(w.pos || '')}${w.is_key ? ' · ★重点' : ''}</span></div>
        ${w.interval_days ? `<div class="rv-meta">间隔 ${w.interval_days} 天 · 错 ${w.error_count} 次</div>` : '<div class="rv-meta">首次学习</div>'}
        <div id="ws-usage"><p class="hint">加载例句…</p></div>
        <div id="ws-nb"></div>
        <div class="rv-actions">
          <button class="btn" onclick="wsResult('fail')">不认识</button>
          <button class="btn accent" onclick="wsResult('pass')">认识</button>
        </div>
      </div>
    </div>
    <button class="btn ghost" onclick="wsQuit()" style="margin-top:8px">返回首页</button>`;
}

// wsFlip 翻面：显示读音释义 + 拉取该词的例句/短语
async function wsFlip() {
  $('ws-front').style.display = 'none';
  $('ws-back').style.display = 'block';
  const w = WS_ITEMS[WS_IDX];
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

async function wsResult(result) {
  const w = WS_ITEMS[WS_IDX];
  try {
    await api('/word-study/' + w.id + '/result', {
      method: 'POST',
      body: JSON.stringify({ result }),
    });
    if (result === 'pass') WS_PASS++; else WS_FAIL++;
  } catch (e) { alert(e.message); return; }
  WS_IDX++;
  renderWordStudyItem();
}

function wsQuit() {
  wsStopAudio();
  loadWordStudyStatus();
  loadHome();
}

function renderWordStudyDone() {
  const total = WS_PASS + WS_FAIL;
  const label = WS_MODE === 'new' ? '新学完成' : '复习完成';
  $('wordstudy-content').innerHTML = `
    <div class="card rv-done-card">
      <h2>${label}</h2>
      <div class="rv-stats">
        <div class="rv-stat pass"><div class="n">${WS_PASS}</div><div class="l">认识</div></div>
        <div class="rv-stat fail"><div class="n">${WS_FAIL}</div><div class="l">不认识</div></div>
        <div class="rv-stat"><div class="n">${total}</div><div class="l">总计</div></div>
      </div>
      <p class="hint" style="text-align:center;margin-top:6px">认识的词明天到期，间隔会逐次翻倍；不认识的明天就会再出现。</p>
      <button class="btn accent" onclick="wsQuit()">返回首页</button>
    </div>`;
  loadWordStudyStatus();
}

// ============ 生词本浏览 ============
let NB_LIST = [], NB_CUR = null, NB_DIRS = [], NB_OPEN = new Set(), NB_WORDS = {}, NB_CURDIR = null;

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
  const sel = $('nb-select'); sel.innerHTML = '';
  NB_LIST.forEach(n => {
    const o = document.createElement('option');
    o.value = n.id; o.textContent = `${n.name}（${n.word_count} 词）`;
    sel.appendChild(o);
  });
  if (!NB_CUR || !NB_LIST.find(x => x.id === NB_CUR)) NB_CUR = NB_LIST[0].id;
  sel.value = NB_CUR;
  await nbLoadTree();
  go('notebook');
}

async function nbSwitch(id) { NB_CUR = id; NB_WORDS = {}; NB_CURDIR = null; await nbLoadTree(); }

async function nbLoadTree() {
  try { NB_DIRS = (await api('/notebooks/' + NB_CUR + '/tree')).dirs || []; } catch (e) { alert(e.message); return; }
  NB_OPEN = new Set(NB_DIRS.filter(d => !d.parent_id).map(d => d.id)); // 默认展开一级
  $('nb-title').textContent = (NB_LIST.find(x => x.id === NB_CUR) || {}).name || '生词本';
  renderNbTree();
  $('nb-words').innerHTML = '';
  $('nb-hint').textContent = '点目录查看该目录下的生词与例句';
}

function nbChildren(pid) {
  return NB_DIRS.filter(d => (d.parent_id || '') === (pid || '')).sort((a, b) => a.sort_order - b.sort_order);
}

function renderNbTree() {
  const box = $('nb-tree'); box.innerHTML = '';
  const walk = (pid, depth) => {
    nbChildren(pid).forEach(d => {
      const kids = nbChildren(d.id);
      const row = document.createElement('div');
      row.className = 'nb-dir' + (NB_CURDIR === d.id ? ' on' : '');
      row.style.paddingLeft = (6 + depth * 16) + 'px';
      row.innerHTML = `<span class="tw">${kids.length ? (NB_OPEN.has(d.id) ? '▾' : '▸') : '·'}</span>` +
        `<span class="nm">${esc(d.name)}</span><span class="ct">${d.word_count || ''}</span>`;
      row.onclick = () => {
        if (kids.length) { NB_OPEN.has(d.id) ? NB_OPEN.delete(d.id) : NB_OPEN.add(d.id); }
        nbOpenDir(d.id);
      };
      box.appendChild(row);
      if (kids.length && NB_OPEN.has(d.id)) walk(d.id, depth + 1);
    });
  };
  walk('', 0);
}

async function nbOpenDir(dirId) {
  NB_CURDIR = dirId;
  renderNbTree();
  const box = $('nb-words'); box.innerHTML = '<p class="hint">加载…</p>';
  let r;
  try { r = await api('/notebooks/dir/' + dirId + '/words'); }
  catch { box.innerHTML = '<p class="hint">加载失败</p>'; return; }
  NB_WORDS[dirId] = r.items || [];
  renderNbWords();
}

function renderNbWords() {
  const items = NB_WORDS[NB_CURDIR] || [];
  const box = $('nb-words'); box.innerHTML = '';
  items.forEach((w, i) => {
    const div = document.createElement('div');
    div.className = 'sent wordrow';
    div.innerHTML = `
      <span class="spk">${i + 1}</span>
      <div class="wrap"><div class="txt"><ruby>${esc(w.surface)}<rt>${esc(w.reading || '')}</rt></ruby></div>
      <div class="zh">${esc(w.meaning || '')} ${esc(w.pos || '')}</div></div>
      <button class="word-speak" title="听单词发音">🔊</button>
      <button class="play" title="展开例句">▾</button>`;
    div.querySelector('.word-speak').onclick = (e) => { e.stopPropagation(); wsSpeakWord(w.word_id, e.currentTarget); };
    div.onclick = () => nbToggleWord(w, div);
    box.appendChild(div);
  });
  $('nb-hint').textContent = `共 ${items.length} 词 · 🔊 听词音 · 点词展开例句（▶ 点播）`;
}

function nbToggleWord(w, div) {
  const ex = div.querySelector('.nb-sents');
  if (ex) { ex.remove(); return; }
  document.querySelectorAll('.nb-sents').forEach(x => x.remove());
  const wrap = document.createElement('div');
  wrap.className = 'nb-sents';
  if (!w.sentences.length) wrap.innerHTML = '<p class="hint" style="margin-left:26px">暂无例句</p>';
  w.sentences.forEach(s => {
    const row = document.createElement('div');
    row.className = 'nb-sent';
    row.innerHTML = `<div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div><div class="zh">${esc(s.translation || '')}</div></div>`;
    const btn = document.createElement('button');
    btn.className = 'play'; btn.textContent = '▶'; btn.title = '点播';
    btn.onclick = (e) => { e.stopPropagation(); sayAudio(s.text, btn); };
    row.appendChild(btn);
    wrap.appendChild(row);
  });
  div.appendChild(wrap);
}

// sayAudio 任意文本发音：/audio/say（服务端 text_audio 缓存）+ 本地 IndexedDB 二级缓存
async function sayAudio(text, btn) {
  try {
    if (btn) btn.textContent = '⏳';
    const key = 'say:' + text;
    let url;
    const hit = await idbGet(key);
    if (hit && hit.blob) {
      url = URL.createObjectURL(hit.blob);
    } else {
      const r = await fetch('/api/v1/audio/say', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + TOKEN },
        body: JSON.stringify({ text }),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const blob = await r.blob();
      idbPut(key, blob);
      url = URL.createObjectURL(blob);
    }
    wsStopAudio();
    WS_AUDIO = new Audio(url);
    WS_AUDIO.onended = WS_AUDIO.onerror = () => { if (btn) btn.textContent = '▶'; };
    await WS_AUDIO.play();
  } catch (e) {
    if (btn) btn.textContent = '▶';
  }
}

// ============ 启动 ============
if (TOKEN) afterLogin(); else { $('userchip').textContent = '未登录'; go('login'); }