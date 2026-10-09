package main

import (
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

// perfReportOptions は **画面(/api/performance と /api/live/performance)の戦績**の
// 組み立て。research と live で別々に書くと片方だけ数え方がずれるので 1 本にまとめる
// (転送漏れで live だけ旧経路に落ちる事故と同じ形)。
//
// 🛑 画面は**口座ベース**。`entry_compensated`(約定後に
// 守りを置けず巻き戻した往復)と `external_close`(人間が建てた建玉の決済)を
// 「戦績の外の別枠」に出すのをやめ、**普通のトレードとして戦略に計上する** —
// 別枠にすると全件がそれだった日に戦績が空に見え、口座で動いた実額が画面から消える。
//
// 🛑 **エッジ判定はここを通らない**。cmd/forward-report は既定(= 戦略の出口だけ)の
// ままで、事前コミットを画面の都合で動かさない。
//
// strategies が nil(in-memory 構成)なら戦略別の内訳が付かないだけ。
func perfReportOptions(strategies port.TradeStrategyResolver) []query.ForwardReportOption {
	opts := []query.ForwardReportOption{query.WithNonStrategyClosesCounted()}
	if strategies != nil {
		opts = append(opts, query.WithStrategyResolver(strategies))
	}
	return opts
}
