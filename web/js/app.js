'use strict';
// 听说 TingShuo 桌面前端（开发版）
// 架构说明：生产 BYOK 形态下，LLM/Azure 由浏览器直连（FR-3.12）；
// 开发期经 /api/v1/dev/* 本地代理转发（生产 DEV_PROXY=false 关闭）。

// ============ 基础 ============
const $ = id => document.getElementById(id);
const tokenKey = 'ts_token', refreshKey = 'ts_refresh';
let TOKEN = localStorage.getItem(tokenKey) || '';
let REFRESH = localStorage.getItem(refreshKey) || '';
let CFG = {};        // /dev/config 结果
let LESSON = null;   // 当前课程全量
let CUR_MAT = 0;     // 当前材料 tab
let CUR_MODE = 'normal'; // 跟读/盲听/译文（重渲染后需恢复）
let VIEW = 'material';   // material=当前材料 / fav=收藏视图
let playing = null;  // 当前 audio
let FAVS = new Set();    // 已收藏句子 ID
let FAV_ITEMS = [];      // 收藏句列表（含内容）
let FAVM = new Set();    // 已收藏材料 ID

function go(name) {
  document.querySelectorAll('.screen').forEach(s => s.classList.remove('on'));
  $('sc-' + name).classList.add('on');
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
  TOKEN = ''; REFRESH = '';
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
      return afterLogin();
    } catch (e) {
      if (path === '/auth/login') return showErr('login-err', e.message);
    }
  }
}
async function afterLogin() {
  $('userchip').textContent = $('login-email').value.trim();
  hideErr('home-err');
  CFG = await api('/dev/config').catch(() => ({}));
  await loadFavs();
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

// goHome 回首页并刷新今日课程状态（学习屏的返回入口）
async function goHome() {
  stopPlay();
  await loadFavs();
  await loadHome();
}

// switchView 学习屏内切换：当前材料 / 生词短语 / 收藏列表
async function switchView(v) {
  if (VIEW === v) return;
  VIEW = v; CUR_MAT = 0;
  if (v === 'fav') await loadFavs();
  $('md-fav').classList.toggle('on', v === 'fav');
  $('md-phrases').classList.toggle('on', v === 'phrases');
  $('md-normal').classList.toggle('on', v === 'material' && CUR_MODE === 'normal');
  if (v === 'fav') renderFavView();
  else if (v === 'phrases') renderPhrasesView();
  else renderLearn();
}

// phrasesMaterial 取当前课程的生词短语材料（type='phrases'）
function phrasesMaterial() {
  return (LESSON.materials || []).find(m => m.type === 'phrases') || null;
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
        <p class="hint">为本课每个生词生成一个自然短语（3—10 词的短句，含该词）——
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
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(s.speaker || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
      <div class="zh">${esc(s.translation || '')}</div></div>
      <button class="fav ${fav ? 'on' : ''}" title="${fav ? '取消收藏' : '收藏句子'}">${fav ? '★' : '☆'}</button>
      <button class="play ${has ? 'has' : ''}" title="${has ? '播放' : '未配音'}">${has ? '▶' : '…'}</button>`;
    div.querySelector('.fav').onclick = (e) => { e.stopPropagation(); toggleFavSentence(s, div); };
    div.onclick = () => playSentence(s, idx, div);
    box.appendChild(div);
  });
  applyMode();
  const withAudio = pm.sentences.filter(s => (s.audios || []).length).length;
  $('learn-hint').textContent = `生词短语 · 共 ${pm.sentences.length} 句 / 已配音 ${withAudio} 句 · 点短语即听 · 「▶ 整段连播」连播全部`;
  $('tts-btn').style.display = 'none';
}

// genPhrases 为本课生词生成短语包（LLM → mode=phrases 上传 → 渲染）
async function genPhrases() {
  hideErr('gen-err');
  const words = LESSON.words || [];
  if (!words.length) return;
  const t0 = Date.now();
  go('gen');
  $('gen-retry').style.display = 'none'; $('gen-back').style.display = 'none';
  $('gen-log').style.display = 'none'; $('gen-log').textContent = '';
  $('gen-title').textContent = '生词短语生成中';
  $('gen-note').innerHTML = '为每个生词写一个自然短语，通常 1—2 分钟。';
  try {
    const prompt = buildPhrasesPrompt(words);
    genLog('调用大模型生成短语…');
    const started = await api('/dev/llm/start', {
      method: 'POST',
      body: JSON.stringify({ model: CFG.llm_model, messages: [{ role: 'user', content: prompt }] }),
    });
    let llmText = '';
    while (true) {
      await new Promise(res2 => setTimeout(res2, 1000));
      let st;
      try { st = await api('/dev/llm/poll?id=' + started.task_id); }
      catch (e2) { continue; }
      llmText = st.text || '';
      const sec = Math.floor(st.elapsed_ms / 1000);
      $('gen-step').textContent = `已等待 ${Math.floor(sec / 60)} 分 ${sec % 60} 秒 · 已输出 ${st.chars || 0} 字符`;
      if (st.err) throw new Error('生成失败：' + st.err);
      if (st.done) break;
    }
    genLog(`生成完毕（${Math.floor((Date.now() - t0) / 1000)} 秒）`);
    const out = JSON.parse(largestJSON(llmText));

    // surface → word_id 重映射（与完整包同规则）
    const idBySurface = {};
    for (const w of words) idBySurface[w.surface] = w.id;
    const occ = {};
    for (const [k, v] of Object.entries(out.word_occurrence || {})) occ[idBySurface[k] || k] = v;
    out.word_occurrence = occ;

    const problems = validateLocally(out, words, false);
    if (problems.length) throw new Error('短语校验未通过：' + problems.join('；'));

    const up = await api(`/lessons/${LESSON.lesson.id}/materials`, {
      method: 'POST',
      body: JSON.stringify({ mode: 'phrases', ...sanitizeOutput(out) }),
    });
    genLog('短语落库成功');
    LESSON = await api('/lessons/' + LESSON.lesson.id);
    VIEW = 'phrases';
    go('learn'); renderPhrasesView();
  } catch (e) {
    showErr('gen-err', e.message);
    $('gen-retry').style.display = 'block';
    $('gen-retry').textContent = '↻ 重新生成';
    $('gen-back').style.display = 'block';
    $('gen-back').textContent = '返回学习';
    $('gen-back').onclick = () => { VIEW = 'phrases'; go('learn'); renderPhrasesView(); };
    $('gen-title').textContent = '短语生成失败';
  }
}

// buildPhrasesPrompt 生词短语包的生成提示（FR-2.15 搭配卡形态）
function buildPhrasesPrompt(words) {
  const lines = words.map((w, i) => `p${i + 1}｜${w.surface}｜${w.reading || '—'}｜${w.meaning}`);
  return `你是日语教学专家。请为下面的每个词生成**恰好一个自然短语**（3—10 个词的短句，必须包含该词），难度贴合初级水平，です・ます体，来自真实生活场景。

要求：
- 每个短语独立成句，自然地道，不用生硬堆砌
- 句子 id 用 p1、p2…（与词汇表顺序对应），sort_order 从 1 开始
- 每句包含 text（日语）、kana（整句假名读音）、translation（中文翻译）、speaker（填该词的 surface）
- word_occurrence：key 是词的 surface，value 是该词所在短语的 id 数组（如 ["p1"]）

词汇表（id｜surface｜读音｜中文）：
${lines.join('\n')}

严格按如下 JSON 结构输出（不要 markdown 围栏、不要解释）：
{"materials":[{"id":"ph1","type":"phrases","scene":"生词短语","title":"生词短语","role_a":null,"role_b":null,"sort_order":99,"sentences":[{"id":"p1","sort_order":1,"speaker":"精算","text":"...","kana":"...","translation":"...","para_no":0}]}],"qa_items":[],"substitution_items":[],"word_occurrence":{"精算":["p1"]}}`;
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
  } else if (todayIt) {
    chip.textContent = '准备中'; chip.classList.remove('ready');
    $('lesson-date').textContent = todayIt.date;
    $('lesson-desc').textContent = '今日课程草稿就绪，可重新提交词单生成。';
    btn.textContent = '继续提交词单';
    btn.onclick = () => go('create');
  } else {
    chip.textContent = '未创建'; chip.classList.remove('ready');
    $('lesson-date').textContent = '—';
    $('lesson-desc').textContent = '还没有今天的课程。1 个词也可以生成一课。';
    btn.textContent = '创建今日课程';
    btn.onclick = () => go('create');
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
    const prompt = buildPrompt(words, $('sel-level').value);
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
    const problems = validateLocally(out, full.words, true);
    if (problems.length) throw new Error('材料校验未通过：' + problems.join('；'));
    genLog('校验通过，上传落库…');

    // 4. 上传（白名单净化后的干净结构）
    const up = await api(`/lessons/${lessonID}/materials`, { method: 'POST', body: JSON.stringify(sanitizeOutput(out)) });
    genLog('落库成功 status=' + up.status + (up.warnings?.length ? '（注音警告 ' + up.warnings.length + ' 句）' : ''));

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
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(s.speaker || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(s.ruby_tokens) || esc(s.text)}</div>
      <div class="zh">${esc(s.translation || '')}</div></div>
      <button class="fav ${fav ? 'on' : ''}" title="${fav ? '取消收藏' : '收藏句子'}">${fav ? '★' : '☆'}</button>
      <button class="play ${has ? 'has' : ''}" title="${has ? '播放' : '未配音'}">${has ? '▶' : '…'}</button>`;
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
    const div = document.createElement('div');
    div.className = 'sent'; div.dataset.idx = idx;
    div.innerHTML = `
      <span class="spk">${esc(it.lesson_date || '')}</span>
      <div class="wrap"><div class="txt">${rubyHTML(it.ruby_tokens) || esc(it.text)}</div>
      <div class="zh">${esc(it.translation || '')}</div></div>
      <button class="fav on" title="取消收藏">★</button>
      <button class="play ${it.audio_id ? 'has' : ''}">${it.audio_id ? '▶' : '…'}</button>`;
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
async function audioURL(s) {
  const a = (s.audios || [])[0];
  if (!a) return null;
  const r = await fetch('/api/v1/audio/' + a.id + '/file',
    { headers: { Authorization: 'Bearer ' + TOKEN } });
  if (!r.ok) throw new Error('音频获取失败 HTTP ' + r.status);
  return URL.createObjectURL(await r.blob());
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

// ============ 启动 ============
if (TOKEN) afterLogin(); else { $('userchip').textContent = '未登录'; go('login'); }
