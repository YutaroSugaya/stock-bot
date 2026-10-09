package broker

import (
	"crypto/rsa"
	"fmt"
	"os"
)

// TachibanaCreds is the 公開鍵認証 material read from the environment.
type TachibanaCreds struct {
	AuthID   string
	SecondPW string
	Key      *rsa.PrivateKey
}

// TachibanaCredsFromEnv reads the 公開鍵認証 material. 発注系は第二暗証番号が要る
// ので requireSecondPW=true、参照専用(probe / fetch-daily)は false。
//
// 5 バイナリが同じ 14 行を手写ししていて、`_FILE` 優先の順序と「壊れた鍵は初回
// login まで遅らせず起動時に落とす」という規律が綴りごとにずれていた。
func TachibanaCredsFromEnv(requireSecondPW bool) (TachibanaCreds, error) {
	c := TachibanaCreds{
		AuthID:   os.Getenv("STOCKBOT_TACHIBANA_AUTH_ID"),
		SecondPW: os.Getenv("STOCKBOT_TACHIBANA_SECOND_PASSWORD"),
	}
	keyPEM := os.Getenv("STOCKBOT_TACHIBANA_PRIVATE_KEY")
	if f := os.Getenv("STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return TachibanaCreds{}, fmt.Errorf("tachibana: 秘密鍵ファイル読み込み: %w", err)
		}
		keyPEM = string(b)
	}
	if c.AuthID == "" || keyPEM == "" {
		return TachibanaCreds{}, fmt.Errorf("要 env: STOCKBOT_TACHIBANA_AUTH_ID / _PRIVATE_KEY(_FILE) (公開鍵認証)")
	}
	if requireSecondPW && c.SecondPW == "" {
		return TachibanaCreds{}, fmt.Errorf("要 env: STOCKBOT_TACHIBANA_SECOND_PASSWORD(発注系は第二暗証番号が必須)")
	}
	key, err := ParseTachibanaPrivateKey(keyPEM)
	if err != nil {
		return TachibanaCreds{}, err
	}
	c.Key = key
	return c, nil
}
