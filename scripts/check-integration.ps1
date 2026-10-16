<#
 * @Author: LiuShuXin
 * @Description: 对显式选择的专用真实依赖执行验收，缺少配置时直接失败
 * @File: check-integration.ps1
 * Software: Goland
 * @Date: 2026/10/15
#>
param(
    [switch]$Postgres,
    [switch]$Redis,
    [switch]$Taskqueue,
    [switch]$Objectstore,
    [switch]$Race
)

$ErrorActionPreference = 'Stop'
$taskPackages = @()
$taskRequired = @()
if ($Postgres) {
    $taskPackages += './postgres'
    $taskRequired += 'POSTGRES_TEST_HOST', 'POSTGRES_TEST_USER', 'POSTGRES_TEST_DATABASE', 'POSTGRES_TEST_SSL_MODE'
}
if ($Redis) {
    $taskPackages += './redis'
    $taskRequired += 'REDIS_TEST_HOST', 'REDIS_TEST_PORT', 'REDIS_TEST_DATABASE', 'REDIS_TEST_TLS'
}
if ($Taskqueue) {
    $taskPackages += './taskqueue'
    $taskRequired += 'TASKQUEUE_TEST_HOST', 'TASKQUEUE_TEST_PORT', 'TASKQUEUE_TEST_DATABASE', 'TASKQUEUE_TEST_TLS'
}
if ($Objectstore) {
    $taskPackages += './objectstore'
    $taskRequired += 'OBJECTSTORE_TEST_ENDPOINT', 'OBJECTSTORE_TEST_BUCKET', 'OBJECTSTORE_TEST_ACCESS_KEY_ID', 'OBJECTSTORE_TEST_ACCESS_KEY_SECRET'
}
if ($taskPackages.Count -eq 0) { throw '请选择 -Postgres、-Redis、-Taskqueue 或 -Objectstore。' }
$taskMissing = @($taskRequired | Where-Object { [string]::IsNullOrEmpty([Environment]::GetEnvironmentVariable($_)) })
if ($taskMissing.Count -ne 0) { throw "真实依赖验收缺少环境变量：$($taskMissing -join ', ')" }

$taskOriginalWork = $env:GOWORK
$taskOriginalQueueFlag = $env:TASKQUEUE_TEST_INTEGRATION
Push-Location (Join-Path $PSScriptRoot '..')
try {
    $env:GOWORK = 'off'
    if ($Taskqueue) { $env:TASKQUEUE_TEST_INTEGRATION = '1' }
    $taskTestArgs = @('-count=1', '-run', '^TestIntegration', '-v')
    if ($Race) { $taskTestArgs += '-race' }
    & go test @taskTestArgs @taskPackages
    if ($LASTEXITCODE -ne 0) { throw '真实依赖验收失败。' }
    Write-Output '所选真实依赖验收通过。'
}
finally {
    $env:GOWORK = $taskOriginalWork
    $env:TASKQUEUE_TEST_INTEGRATION = $taskOriginalQueueFlag
    Pop-Location
}
