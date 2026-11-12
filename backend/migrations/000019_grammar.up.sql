-- 语法本：按教材课次组织的全局语法内容（例句发音走 TTS，不存音频）。
-- 与生词本/课文本的联动不在库内建外键：内容全局，例句按归一化文本实时匹配当前用户的例句与课文行。
BEGIN;

CREATE TABLE grammar_lesson (
    book       TEXT NOT NULL,              -- 初级上 / 初级下
    lesson_no  INT NOT NULL,
    title      TEXT NOT NULL,
    sort_order INT NOT NULL DEFAULT 0,
    PRIMARY KEY (book, lesson_no)
);

CREATE TABLE grammar_point (
    id         TEXT PRIMARY KEY,
    book       TEXT NOT NULL,
    lesson_no  INT NOT NULL,
    seq        INT NOT NULL,
    section    TEXT NOT NULL DEFAULT 'grammar',  -- grammar=语法解释 / expression=表达扩展
    pattern    TEXT NOT NULL,              -- 句型标题，如「名词句（敬体）的过去时」
    formation  TEXT NOT NULL DEFAULT '',   -- 接续摘要
    body       JSONB NOT NULL DEFAULT '[]'::jsonb,  -- [{type:'p',text} | {type:'table',title,head,rows}]
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (book, lesson_no, seq),
    FOREIGN KEY (book, lesson_no) REFERENCES grammar_lesson (book, lesson_no) ON DELETE CASCADE
);

CREATE TABLE grammar_example (
    id          TEXT PRIMARY KEY,
    point_id    TEXT NOT NULL REFERENCES grammar_point (id) ON DELETE CASCADE,
    seq         INT NOT NULL,
    speaker     TEXT NOT NULL DEFAULT '',  -- A / B（对话例句），空=独白
    mark        TEXT NOT NULL DEFAULT '',  -- ○ 正确 / × 误用
    text        TEXT NOT NULL,
    translation TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    ruby_tokens JSONB,
    UNIQUE (point_id, seq)
);
CREATE INDEX grammar_example_point_idx ON grammar_example (point_id, seq);

COMMIT;
