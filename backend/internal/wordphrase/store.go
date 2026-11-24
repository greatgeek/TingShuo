package wordphrase

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Load 读取缓存词组；无缓存或结构异常返回 ok=false。
func Load(ctx context.Context, pool *pgxpool.Pool, wordID string) ([]Item, bool) {
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM word_phrase WHERE word_id = $1`, wordID).Scan(&raw); err != nil {
		return nil, false
	}
	var items []Item
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, false
	}
	return items, true
}

// Save 写入/更新缓存词组。
func Save(ctx context.Context, pool *pgxpool.Pool, wordID string, items []Item, model string) error {
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO word_phrase (word_id, payload, model)
		VALUES ($1, $2, $3)
		ON CONFLICT (word_id) DO UPDATE
		  SET payload = EXCLUDED.payload, model = EXCLUDED.model, created_at = now()`,
		wordID, b, model)
	return err
}