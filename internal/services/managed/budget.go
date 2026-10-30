package managed

import "songloft/internal/services/resourcebudget"

// S-05 进程级资源预算实现位于 resourcebudget（供各服务共用，避免 services 与
// managed 之间形成导入环）。这里保留 managed 侧的历史 API 别名。

type Budget = resourcebudget.Budget
type PartitionLimits = resourcebudget.PartitionLimits

// PartitionCache 是 managed 准备链转码使用的预算分区名。
const PartitionCache = resourcebudget.PartitionCache

var ErrBudgetFull = resourcebudget.ErrBudgetFull

func DefaultBudget() *Budget { return resourcebudget.DefaultBudget() }

func ProcessBudget() *Budget { return resourcebudget.ProcessBudget() }
