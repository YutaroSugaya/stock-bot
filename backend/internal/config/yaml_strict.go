package config

import (
	"bytes"
	"errors"
	"io"

	"gopkg.in/yaml.v3"
)

// decodeStrict は **知らないキーを拒否する** yaml の読み込み。
//
// `yaml.Unmarshal` は未知キーを黙って捨てるので、綴りを間違えた安全装置や、対応する
// フィールドの無い設定(live の `holding.default_mode` が実例)が「効いているように見えて
// 何もしていない」状態で残る。空のファイルは Unmarshal と同じく読めたことにする。
func decodeStrict(b []byte, v any) error {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
