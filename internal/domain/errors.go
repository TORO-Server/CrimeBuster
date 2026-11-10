package domain

import (
	"errors"
)

var (
	// ErrForbidden は、してはいけない相手 (信頼する人・op) や、受け付けない動かし方のとき。
	ErrForbidden = errors.New("forbidden")
	// ErrConflict は、すでにやった・いま別の操作が動いているなど、いまはできないとき。
	ErrConflict = errors.New("conflict")
	// ErrUnavailable は、必要な部品 (コンソール・Discord・ブラウザ) が使えない設定のとき。
	ErrUnavailable = errors.New("unavailable")
)
