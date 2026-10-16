/**
 * @Author: LiuShuXin
 * @Description: 状态查询的命令 Context 适配和请求关联边界
 * @File: context.go
 * Software: Goland
 * @Date: 2026/10/15
 */
package taskqueue

import (
	"context"
	"regexp"

	goredis "github.com/redis/go-redis/v9"
)

type boundRedis struct {
	goredis.UniversalClient
	ctx context.Context
}

func (c boundRedis) SIsMember(_ context.Context, key string, member any) *goredis.BoolCmd {
	return c.UniversalClient.SIsMember(c.ctx, key, member)
}
func (c boundRedis) Eval(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	return c.UniversalClient.Eval(c.ctx, script, keys, args...)
}
func (c boundRedis) EvalSha(_ context.Context, sha string, keys []string, args ...any) *goredis.Cmd {
	return c.UniversalClient.EvalSha(c.ctx, sha, keys, args...)
}
func (c boundRedis) EvalRO(_ context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	return c.UniversalClient.EvalRO(c.ctx, script, keys, args...)
}
func (c boundRedis) EvalShaRO(_ context.Context, sha string, keys []string, args ...any) *goredis.Cmd {
	return c.UniversalClient.EvalShaRO(c.ctx, sha, keys, args...)
}
func (c boundRedis) ScriptExists(_ context.Context, hashes ...string) *goredis.BoolSliceCmd {
	return c.UniversalClient.ScriptExists(c.ctx, hashes...)
}
func (c boundRedis) ScriptLoad(_ context.Context, script string) *goredis.StringCmd {
	return c.UniversalClient.ScriptLoad(c.ctx, script)
}

var correlationPattern = regexp.MustCompile(`^req_[a-z2-7]{20}$`)

func validCorrelation(value string) bool {
	return correlationPattern.MatchString(value) || validID(value)
}
