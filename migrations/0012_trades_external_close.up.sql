-- trades.close_reason に 'external_close' を足す。
--
-- external(証券アプリ等で人間が建てた)建玉が broker から消えたとき、reconcile は
-- MarkClosed するだけで trade 行を作っていなかった。「その PnL は bot のものではない」
-- のは**戦略成績としては**正しいが、**口座には実額として効いている**(委託保証金・
-- 維持率・日次損失に影響する)。台帳から永久に消えるのは監査上おかしい。
--
-- 戦略の出口ではないので、エッジ判定では close_reason で除外する
-- (entry_compensated と同じ扱い)。
ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_close_reason_check;
ALTER TABLE trades ADD CONSTRAINT trades_close_reason_check CHECK (close_reason IN
    ('take_profit','stop_loss','max_hold','early_exit','ratchet_takeprofit',
     'manual','reconcile_cold_close','broker_close','forced_flat','entry_compensated',
     'external_close'));
