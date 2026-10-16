/**
 * @Author: LiuShuXin
 * @Description: Redis 命令类型与有界 Pipeline 摘要诊断，避免泄露参数
 * @File: diagnostic.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cauliflower-beep/easy-go-framework/logging"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// 命令名也可能来自 Pipeline.Do 的任意输入。只输出已知的 Redis 标准命令，
// 不把未知模块/自定义命令名、子命令或 Args/String 的内容带入日志。
var diagnosticCommands = func() map[string]string {
	commands := make(map[string]string)
	for _, name := range strings.Fields(`
		ACL APPEND ASKING AUTH BGREWRITEAOF BGSAVE BITCOUNT BITFIELD BITFIELD_RO BITOP BITPOS
		BLMOVE BLMPOP BLPOP BRPOP BRPOPLPUSH BZMPOP BZPOPMAX BZPOPMIN
		CLIENT CLUSTER COMMAND CONFIG COPY DBSIZE DECR DECRBY DEL DISCARD DUMP ECHO
		EVAL EVAL_RO EVALSHA EVALSHA_RO EXEC EXISTS EXPIRE EXPIREAT EXPIRETIME FAILOVER
		FCALL FCALL_RO FLUSHALL FLUSHDB FUNCTION GEOADD GEODIST GEOHASH GEOPOS
		GEORADIUS GEORADIUS_RO GEORADIUSBYMEMBER GEORADIUSBYMEMBER_RO GEOSEARCH GEOSEARCHSTORE
		GET GETBIT GETDEL GETEX GETRANGE GETSET
		HDEL HEXISTS HEXPIRE HEXPIREAT HEXPIRETIME HGET HGETALL HGETDEL HGETEX
		HINCRBY HINCRBYFLOAT HKEYS HLEN HMGET HMSET HPERSIST HPEXPIRE HPEXPIREAT HPEXPIRETIME
		HPTTL HRANDFIELD HSCAN HSET HSETEX HSETNX HSTRLEN HTTL HVALS
		HELLO INCR INCRBY INCRBYFLOAT INFO KEYS LASTSAVE LATENCY
		LINDEX LINSERT LLEN LMOVE LMPOP LOLWUT LPOP LPOS LPUSH LPUSHX LRANGE LREM LSET LTRIM
		MEMORY MGET MIGRATE MODULE MONITOR MOVE MSET MSETNX MULTI OBJECT
		PERSIST PEXPIRE PEXPIREAT PEXPIRETIME PFADD PFCOUNT PFMERGE PING PSETEX PSUBSCRIBE
		PTTL PUBLISH PUBSUB PUNSUBSCRIBE QUIT RANDOMKEY READONLY READWRITE RENAME RENAMENX
		REPLICAOF RESTORE ROLE RPOP RPOPLPUSH RPUSH RPUSHX
		SADD SAVE SCAN SCARD SCRIPT SDIFF SDIFFSTORE SELECT SET SETBIT SETEX SETNX SETRANGE
		SHUTDOWN SINTER SINTERCARD SINTERSTORE SISMEMBER SLAVEOF SLOWLOG SMEMBERS SMISMEMBER
		SMOVE SORT SORT_RO SPOP SPUBLISH SRANDMEMBER SREM SSCAN SSUBSCRIBE STRLEN SUBSCRIBE
		SUBSTR SUNION SUNIONSTORE SUNSUBSCRIBE SWAPDB SYNC TIME TOUCH TTL TYPE
		UNLINK UNSUBSCRIBE UNWATCH WAIT WAITAOF WATCH
		XACK XACKDEL XADD XAUTOCLAIM XCLAIM XDEL XDELEX XGROUP XINFO XLEN XPENDING
		XRANGE XREAD XREADGROUP XREVRANGE XSETID XTRIM
		ZADD ZCARD ZCOUNT ZDIFF ZDIFFSTORE ZINCRBY ZINTER ZINTERCARD ZINTERSTORE ZLEXCOUNT
		ZMPOP ZMSCORE ZPOPMAX ZPOPMIN ZRANDMEMBER ZRANGE ZRANGEBYLEX ZRANGEBYSCORE
		ZRANGESTORE ZRANK ZREM ZREMRANGEBYLEX ZREMRANGEBYRANK ZREMRANGEBYSCORE
		ZREVRANGE ZREVRANGEBYLEX ZREVRANGEBYSCORE ZREVRANK ZSCAN ZSCORE ZUNION ZUNIONSTORE
	`) {
		commands[strings.ToLower(name)] = name
	}
	return commands
}()

func diagnosticCommand(cmd goredis.Cmder) string {
	if name, ok := diagnosticCommands[cmd.Name()]; ok {
		return name
	}
	return "OTHER"
}

const maxDiagnosticCommandTypes = 8

type commandTypeCount struct {
	Command string `json:"command"`
	Count   int    `json:"count"`
}

func commandFields(cmds []goredis.Cmder) []zap.Field {
	fields := []zap.Field{zap.Int("command_count", len(cmds))}
	if len(cmds) == 1 {
		return append(fields, zap.String("command", diagnosticCommand(cmds[0])))
	}
	// 保留首次出现顺序；最多聚合八种命令，避免大批次产生无界日志。
	types := make([]commandTypeCount, 0, maxDiagnosticCommandTypes)
	omitted := 0
	for _, cmd := range cmds {
		name := diagnosticCommand(cmd)
		found := false
		for i := range types {
			if types[i].Command == name {
				types[i].Count++
				found = true
				break
			}
		}
		if !found {
			if len(types) < maxDiagnosticCommandTypes {
				types = append(types, commandTypeCount{Command: name, Count: 1})
			} else {
				omitted++
			}
		}
	}
	fields = append(fields, zap.Any("command_types", types))
	if omitted > 0 {
		fields = append(fields, zap.Int("omitted_command_count", omitted))
	}
	return fields
}

func logCommandOutcome(logger *zap.Logger, ctx context.Context, err error, elapsed time.Duration, slow time.Duration, cmds []goredis.Cmder) {
	failed := err != nil && !errors.Is(err, Nil)
	if !failed && (slow <= 0 || elapsed < slow) {
		return
	}
	// 成功且不慢的命令不构建摘要，减少正常调用的诊断开销。
	fields := append([]zap.Field{zap.Duration("duration", elapsed)}, commandFields(cmds)...)
	log := logging.ForRequest(logger, ctx)
	if failed {
		log.Warn("Redis command failed", append(fields, logging.ErrorFields(SafeError(err))...)...)
	} else {
		log.Warn("Redis command slow", fields...)
	}
}
