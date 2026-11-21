// 从 NAS 练口语 mp4 抽取 mp3（mono 24kHz 48kbps）；第40课两段拼接成一课音频
// 用法: node extract_audio.js [lesson,lesson,...]
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const NAS = '//DXP4800-F444/openclaw/合集/清华梁爽：0-N1日语精讲高级班';
const SRC = 'E:/tmp/rec-src';
const OUT = 'out/audio';
const FFMPEG = 'G:/PySceneDetect/ffmpeg.exe';
const FFPROBE = 'D:/ffmpeg/ffmpeg-2021-04-04-git-b1b7cc698b-full_build/bin/ffprobe.exe';

const only = process.argv[2] ? new Set(process.argv[2].split(',').map(Number)) : null;

function duration(file) {
  const out = execFileSync(FFPROBE, ['-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', file], { encoding: 'utf8' });
  return Math.round(Number(out.trim()) * 1000) / 1000;
}

function toMp3(src, dst) {
  execFileSync(FFMPEG, ['-y', '-v', 'error', '-i', src, '-vn', '-ac', '1', '-ar', '24000', '-b:a', '48k', '-codec:a', 'libmp3lame', dst]);
}

fs.mkdirSync(SRC, { recursive: true });
fs.mkdirSync(OUT, { recursive: true });
const files = fs.readdirSync(NAS).filter(f => /\.mp4$/i.test(f));
const parts = [];
for (const f of files) {
  const m = f.match(/【0?(\d+)练口语(?:\s*(\d+))?】/);
  if (!m) continue;
  const lesson = Number(m[1]);
  if (only && !only.has(lesson)) continue;
  parts.push({ lesson, part: m[2] ? Number(m[2]) : null, file: f });
}
parts.sort((a, b) => a.lesson - b.lesson || (a.part || 0) - (b.part || 0));

const metaFile = 'out/audio-meta.json';
const meta = fs.existsSync(metaFile) ? JSON.parse(fs.readFileSync(metaFile, 'utf8')) : {};
const byLesson = new Map();
for (const p of parts) {
  const key = p.part ? `${p.lesson}-${p.part}` : String(p.lesson);
  const local = path.join(SRC, `rec-${key}.mp4`);
  if (!fs.existsSync(local)) fs.copyFileSync(path.join(NAS, p.file), local);
  const mp3 = path.join(SRC, `rec-${key}.mp3`);
  toMp3(local, mp3);
  meta[key] = duration(mp3);
  console.log(`${key}\t${meta[key].toFixed(2)}s\t${p.file}`);
  if (!byLesson.has(p.lesson)) byLesson.set(p.lesson, []);
  byLesson.get(p.lesson).push(mp3);
}

for (const [lesson, mp3s] of byLesson) {
  const dst = path.join(OUT, `lesson-${String(lesson).padStart(2, '0')}.mp3`);
  if (mp3s.length === 1) {
    fs.copyFileSync(mp3s[0], dst);
  } else {
    const list = path.join(SRC, `concat-${lesson}.txt`);
    fs.writeFileSync(list, mp3s.map(f => `file '${f.replace(/\\/g, '/')}'`).join('\n'));
    execFileSync(FFMPEG, ['-y', '-v', 'error', '-f', 'concat', '-safe', '0', '-i', list, '-c', 'copy', dst]);
  }
  console.log(`=> ${dst}\t${duration(dst).toFixed(2)}s`);
}
fs.writeFileSync(metaFile, JSON.stringify(meta, null, 1));
console.log('audio-meta.json 已更新');
