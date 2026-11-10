package domain

// MaxRollbackAge は、コネクタ経由と自動のロールバックが受ける事件の古さの上限 (秒)。
// t: は「いまからさかのぼる」ので、古い事件ほど、そのあとの正当な作業も巻き込む。
const MaxRollbackAge = 24 * 3600

// MaxRollbackSeconds は、コンソールに送る t: の上限 (秒)。中継もこの値で断る。
const MaxRollbackSeconds = MaxRollbackAge + 600

// MaxBlueMapRadius は、1 回に描き直す半径の上限。範囲はプレイヤーの行動で決まるので、広がりすぎないようにする。
const MaxBlueMapRadius = 256
