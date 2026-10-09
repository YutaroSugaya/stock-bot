package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"stockbot/backend/internal/port"
)

// paperBook is the consumer view of a broker whose 建玉帳 lives in the process
// (paper / paper_live_feed). 実ブローカー(立花)は自分が建玉の正本なので実装しない。
type paperBook interface {
	AdoptOpenPositions([]port.BrokerPosition) int
}

// adoptPaperBook restores the paper broker's position book from the durable
// ledger at startup. 実ブローカーでは no-op。
//
// 紙の帳簿はプロセス内 map なので再起動で消えるのに、Postgres の positions は OPEN
// のまま残る。復元しないと再起動を跨いだ建玉で
//
//  1. TP/SL 発火時の ClosePosition が "position not found" → reject 扱いで emergency
//     trip、行は CLOSING のまま座礁する
//  2. 採番が毎プロセス 1 から振り直されるため新しい建玉が古い broker_position_id を
//     再利用し、決済時に**別銘柄の建玉**を閉じて台帳へ別銘柄の値段が記録される
//
// trades は forward 検証の唯一のエッジ証拠なので、2 の汚染は黙って進めてはいけない。
// 一覧の取得失敗も同じ穴なので fail-close(起動を止める)。
func adoptPaperBook(ctx context.Context, brk any, repo port.PositionRepository, logger *slog.Logger) error {
	pb, ok := brk.(paperBook)
	if !ok {
		return nil // 実ブローカー: 建玉の正本は broker 側
	}
	open, err := repo.ListOpenAllSymbols(ctx)
	if err != nil {
		return fmt.Errorf("紙帳簿の復元: 台帳の建玉一覧が読めない: %w", err)
	}
	if len(open) == 0 {
		return nil
	}
	// 紙の帳簿は id を鍵にする map なので、重複したまま走ると決済時に**別銘柄の建玉**
	// が閉じられ、その値段で trades に記録される。汚れた台帳で走り続けるより起動しない
	// 方が安全。
	seen := map[string][]string{} // brokerPositionID -> ["<posID> <symbol>", ...]
	for _, p := range open {
		if p.BrokerPositionID == "" {
			continue
		}
		seen[p.BrokerPositionID] = append(seen[p.BrokerPositionID], fmt.Sprintf("id=%d %s", p.ID, p.Symbol))
	}
	var dup []string
	for id, owners := range seen {
		if len(owners) > 1 {
			dup = append(dup, fmt.Sprintf("%s → %v", id, owners))
		}
	}
	if len(dup) > 0 {
		sort.Strings(dup)
		return fmt.Errorf("紙帳簿の復元: broker_position_id が台帳内で重複している(決済時に別銘柄の建玉を閉じ、"+
			"その値段で trades に記録される)。`make migrate-up`(0005)で採番し直してから起動する。重複: %s",
			strings.Join(dup, " / "))
	}

	bps := make([]port.BrokerPosition, 0, len(open))
	for _, p := range open {
		bps = append(bps, port.BrokerPosition{
			BrokerPositionID: p.BrokerPositionID, Symbol: p.Symbol,
			Side: p.Side, Quantity: p.Quantity, EntryPrice: p.EntryPrice,
		})
	}
	n := pb.AdoptOpenPositions(bps)
	logger.Info("restored paper position book from the ledger",
		"open_in_ledger", len(open), "adopted", n)
	return nil
}
