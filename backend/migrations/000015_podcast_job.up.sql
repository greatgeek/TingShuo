-- 播客后台生成任务（v0.4.0）：长片一集要跑十几分钟，浏览器不能挂着等
-- （手机锁屏/切后台就断），改服务端 worker 跑，前端轮询进度，关页面再回来还能续上。
CREATE TABLE podcast_job (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES "user" (id),
    lesson_id   TEXT REFERENCES daily_lesson (id),
    dir_id      TEXT REFERENCES notebook_dir (id),
    status      TEXT NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued','writing','synthesizing','done','error')),
    stage       TEXT NOT NULL DEFAULT '',        -- 人话进度，前端直接显示
    words       INT NOT NULL DEFAULT 0,          -- 原料词数（用于时长/成本估算）
    seg         INT NOT NULL DEFAULT 0,          -- 写稿段进度
    segs        INT NOT NULL DEFAULT 0,
    round       INT NOT NULL DEFAULT 0,          -- 合成轮进度
    rounds      INT NOT NULL DEFAULT 0,
    episode_id  TEXT REFERENCES podcast_episode (id),
    error       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_podjob_user ON podcast_job (user_id, created_at DESC);
CREATE INDEX idx_podjob_open ON podcast_job (status, created_at)
    WHERE status IN ('queued','writing','synthesizing');
