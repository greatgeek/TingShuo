-- 学习时长：前台主动学习 + 前台听力（页面可见且窗口聚焦）。
-- 计分以服务端为准：每次心跳按距上次计分的时间并集累加（多设备/多标签不重复计），
-- 单日总上限 8 小时、纯听力上限 3 小时；日界线按东八区。
BEGIN;

CREATE TABLE study_day (
    user_id        TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    day            DATE NOT NULL,
    active_seconds INTEGER NOT NULL DEFAULT 0 CHECK (active_seconds >= 0),
    listen_seconds INTEGER NOT NULL DEFAULT 0 CHECK (listen_seconds >= 0),
    last_credit_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);

CREATE TABLE study_feature_day (
    user_id  TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    day      DATE NOT NULL,
    feature  TEXT NOT NULL,
    seconds  INTEGER NOT NULL DEFAULT 0 CHECK (seconds >= 0),
    PRIMARY KEY (user_id, day, feature)
);

CREATE TABLE study_setting (
    user_id        TEXT PRIMARY KEY REFERENCES "user" (id) ON DELETE CASCADE,
    daily_goal_min INTEGER NOT NULL DEFAULT 15 CHECK (daily_goal_min BETWEEN 5 AND 240),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE study_milestone (
    user_id     TEXT NOT NULL REFERENCES "user" (id) ON DELETE CASCADE,
    key         TEXT NOT NULL,
    achieved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

COMMIT;