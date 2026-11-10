package config

// Capabilities は、動かし方 (Mode) ごとに「何をしてよいか」を表す能力の集合。
// モードから決まる可否はここだけで定義し、各処理はモードそのものではなく能力を見る。
// 部品の有無 (reporter や ops の nil) と、個別の設定 (KickEnabled やミュートのコマンド) は
// 能力には含めない。それらは従来どおり、各箇所で能力と合わせて判定する。
type Capabilities struct {
	RecordOnly   bool // 検知と記録だけを行う (撮影・報告文の生成・投稿・操作の受け付けをしない)
	Capture      bool // 撮影と報告文の生成を行う
	AcceptOps    bool // コネクタ経由の操作 (通報・ロールバック・却下・ミュート・キック) を受け付ける
	AutoReport   bool // 検知の処理の中で、自動で Discord へ投稿する
	AutoRollback bool // 検知の処理の中で、自動でロールバックする
	AutoMute     bool // チャットの検知で、自動でミュートする
	AutoKick     bool // 検知したときに、自動でキックする
	// KeepEditCommandUsers は、編集コマンドの使用者を対象外にせず、事件として扱うか。
	// review だけが true で、それ以外は使用者を対象外にする。
	KeepEditCommandUsers bool
	DryRunNotice         bool // 報告文に「試運転」の注記を付ける
}

// CapabilitiesFor は、モードに対応する能力の集合を返す。モードの意味はこの関数だけで決まる。
// 知らないモードには能力を何も与えない (loadConfig が不正なモードを弾くため、通常は到達しない)。
func CapabilitiesFor(m Mode) Capabilities {
	switch m {
	case ModeObserve:
		return Capabilities{RecordOnly: true}
	case ModeDryRun:
		return Capabilities{Capture: true, DryRunNotice: true}
	case ModeReview:
		return Capabilities{Capture: true, AcceptOps: true, KeepEditCommandUsers: true}
	case ModeLive:
		return Capabilities{
			Capture: true, AcceptOps: true,
			AutoReport: true, AutoRollback: true, AutoMute: true, AutoKick: true,
		}
	}
	return Capabilities{}
}
