package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

func openPos(t *testing.T, repo port.PositionRepository, sym string) {
	t.Helper()
	if _, err := repo.Insert(context.Background(), port.PositionInsertInput{
		Symbol: sym, Side: order.SideBuy, Quantity: 100, EntryPrice: 3000,
		BrokerPositionID: "bp-" + sym, OpenedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Insert %s: %v", sym, err)
	}
}

// **ユニバースから外れた銘柄の建玉が管理外にならないこと。** 出口(TP/SL・max_hold・
// 14:50 の強制フラット化・reconcile)は全て bundle の中にあり、bundle は
// botCfg.Symbols のぶんしか作られないので、合流させないと**永久に決済されない**。
func TestWatchedSymbolsAdoptsOpenPositionsOutsideUniverse(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	openPos(t, repo, "6976") // 今日のユニバースには居ないが建玉あり
	openPos(t, repo, "7203") // ユニバース内(重複させない)

	all, adopted, err := watchedSymbols(context.Background(), repo, []string{"7203", "6758"})
	if err != nil {
		t.Fatalf("watchedSymbols: %v", err)
	}
	if strings.Join(all, ",") != "7203,6758,6976" {
		t.Fatalf("監視対象 = %v, want [7203 6758 6976](ユニバースの順序を保ち、建玉銘柄を末尾に足す)", all)
	}
	if len(adopted) != 1 || adopted[0] != "6976" {
		t.Fatalf("合流した銘柄 = %v, want [6976]", adopted)
	}
}

func TestWatchedSymbolsNoOpWhenAllHeldSymbolsAreInUniverse(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	openPos(t, repo, "7203")
	all, adopted, err := watchedSymbols(context.Background(), repo, []string{"7203", "6758"})
	if err != nil {
		t.Fatalf("watchedSymbols: %v", err)
	}
	if len(all) != 2 || len(adopted) != 0 {
		t.Fatalf("all=%v adopted=%v — 合流は不要のはず", all, adopted)
	}
}

// 同じ状態から同じ監視対象が出ること(建玉の並び順に依存しない)= 再現性の前提。
func TestWatchedSymbolsIsDeterministic(t *testing.T) {
	repo := repository.NewInMemoryPositionRepo()
	for _, s := range []string{"9984", "6976", "8306"} {
		openPos(t, repo, s)
	}
	for i := 0; i < 10; i++ {
		all, adopted, err := watchedSymbols(context.Background(), repo, []string{"7203"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(all, ",") != "7203,6976,8306,9984" {
			t.Fatalf("合流は銘柄コード昇順で決定論: %v", all)
		}
		if strings.Join(adopted, ",") != "6976,8306,9984" {
			t.Fatalf("adopted = %v", adopted)
		}
	}
}

// 「建玉が無いことにして起動する」と、その建玉は誰にも決済されないまま残る。
func TestWatchedSymbolsFailsClosedWhenRepoErrors(t *testing.T) {
	_, _, err := watchedSymbols(context.Background(), errRepo{}, []string{"7203"})
	if err == nil {
		t.Fatal("建玉一覧の取得に失敗したら起動拒否にすること")
	}
}

// errRepo は建玉一覧だけが失敗する repo。MOCK rationale: 障害注入(DB 断)。
type errRepo struct{ port.PositionRepository }

func (errRepo) ListOpenAllSymbols(context.Context) ([]position.Position, error) {
	return nil, errors.New("db down")
}
