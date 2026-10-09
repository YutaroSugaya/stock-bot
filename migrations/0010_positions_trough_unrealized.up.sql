-- 0010: 建玉の最大逆行(MAE)を記録する列。peak_unrealized_jpy(0006)と対になる。
--
-- なぜ: トレールが効いたかどうかは「もしトレールだったらどこで決済されていたか」で
-- しか測れず、それには各建玉の最大順行(peak)と最大逆行(trough)が要る。peak は
-- 0006 で列だけあったが、更新が ratchet 分岐の中にしか無かったため非ゼロの行は
-- ほとんど無く、反実仮想は 5 分足からの再構築(誤差数%)に頼るしかなかった。
-- 全建玉の更新に変える。
--
-- trough は「SL がもう少し広ければ助かったか」= MAE 側の材料。単位は peak と同じ
-- 円/株(建値からの含み)で、逆行なので **負値**。0 = 一度も建値を割っていない。
--
-- **決済規則は一切変わらない**(どの exit も trough を読まない)。純粋な計測器の追加。

ALTER TABLE positions ADD COLUMN IF NOT EXISTS trough_unrealized_jpy double precision NOT NULL DEFAULT 0;
