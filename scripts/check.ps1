<#
 * @Author: LiuShuXin
 * @Description: 独立验证公共框架格式、静态检查、测试与库构建
 * @File: check.ps1
 * Software: Goland
 * @Date: 2026/10/10 18:00
#>

param([switch]$Race)

$ErrorActionPreference = 'Stop'
$taskOriginalWork = $env:GOWORK
Push-Location (Join-Path $PSScriptRoot '..')
try {
    & go version
    if ($LASTEXITCODE -ne 0) { throw 'Go 工具链不可用。' }
    $taskGoRoot = & go env GOROOT
    if ($LASTEXITCODE -ne 0) { throw '无法定位 Go 工具链。' }
    $taskSuffix = if ($env:OS -eq 'Windows_NT') { '.exe' } else { '' }
    $taskFormatter = Join-Path $taskGoRoot "bin/gofmt$taskSuffix"
    $taskModuleArgs = @()
    # 框架检查不依赖父工作区或业务模块。
    $env:GOWORK = 'off'

    $taskDirectories = @(& go list @taskModuleArgs -f '{{.Dir}}' ./...)
    if ($LASTEXITCODE -ne 0 -or $taskDirectories.Count -eq 0) { throw '无法发现 Go 包。' }
    $taskUnformatted = @(& $taskFormatter -l @taskDirectories)
    if ($LASTEXITCODE -ne 0) { throw '格式检查失败。' }
    if ($taskUnformatted.Count -ne 0) { throw "请格式化文件：$($taskUnformatted -join ', ')" }
    & go vet @taskModuleArgs ./...
    if ($LASTEXITCODE -ne 0) { throw '静态检查失败。' }
    $taskTestArgs = @('-count=1')
    if ($Race) { $taskTestArgs += '-race' }
    & go test @taskModuleArgs @taskTestArgs ./...
    if ($LASTEXITCODE -ne 0) { throw '测试失败。' }
    & go build ./...
    if ($LASTEXITCODE -ne 0) { throw '公共库构建失败。' }
    Write-Output '公共框架检查通过。'

}
finally {
    $env:GOWORK = $taskOriginalWork
    Pop-Location
}
