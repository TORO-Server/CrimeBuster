package domain

// MaxRollbackAge は、コネクタ経由と自動のロールバックが受ける事件の古さの上限 (秒)。
// t: は「いまからさかのぼる」ので、古い事件ほど、そのあとの正当な作業も巻き込む。
const MaxRollbackAge = 24 * 3600

// MaxRollbackSeconds は、コンソールに送る t: の上限 (秒)。許可リストもこの値で断る。
const MaxRollbackSeconds = MaxRollbackAge + 600
