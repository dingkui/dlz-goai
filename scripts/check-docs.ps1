# Run from any directory with PowerShell 7 and Go 1.24+.
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
Push-Location $repoRoot
try {
    $files = @((Get-Item -LiteralPath 'README.md')) + @(Get-ChildItem -LiteralPath 'docs' -Recurse -Filter '*.md')
    $issues = [Collections.Generic.List[string]]::new()
    $programs = [Collections.Generic.List[string]]::new()
    foreach ($file in $files) {
        $body = [IO.File]::ReadAllText($file.FullName)
        if ($body.IndexOf([char]0) -ge 0) { $issues.Add("NUL byte: $($file.FullName)") }
        if (([regex]::Matches($body, '(?m)^```').Count % 2) -ne 0) {
            $issues.Add("Unbalanced fenced blocks: $($file.FullName)")
        }
        # Check local inline Markdown links outside fenced blocks. Remote URLs
        # and heading fragments are deliberately not validated by this script.
        $prose = [regex]::Replace($body, '(?ms)^```[^\r\n]*\r?\n.*?^```[ \t]*\r?$', '')
        foreach ($link in [regex]::Matches($prose, '\]\(([^)]+)\)')) {
            $target = $link.Groups[1].Value
            if ($target -match '^(https?://|mailto:|#)') { continue }
            $relative = [Uri]::UnescapeDataString(($target -split '#')[0])
            if (-not (Test-Path -LiteralPath (Join-Path $file.DirectoryName $relative))) {
                $issues.Add("Broken link: $($file.Name) -> $target")
            }
        }
        foreach ($block in [regex]::Matches($body, '(?ms)^```go\r?\n(.*?)^```[ \t]*\r?$')) {
            $code = $block.Groups[1].Value
            if ($code -match '(?m)^package main\s*$') { $programs.Add($code) }
        }
    }
    if ($issues.Count -gt 0) { throw ($issues -join [Environment]::NewLine) }
    & go build -mod=readonly ./examples/...
    if ($LASTEXITCODE -ne 0) { throw 'Example packages did not compile' }
    $scratch = Join-Path ([IO.Path]::GetTempPath()) ('dlz-docs-' + [guid]::NewGuid().ToString('N'))
    [IO.Directory]::CreateDirectory($scratch) | Out-Null
    for ($i = 0; $i -lt $programs.Count; $i++) {
        $source = Join-Path $scratch "example$i.go"
        [IO.File]::WriteAllText($source, $programs[$i], [Text.UTF8Encoding]::new($false))
        & go build -mod=readonly -o (Join-Path $scratch "example$i.exe") $source
        if ($LASTEXITCODE -ne 0) { throw "README/docs standalone program $i did not compile" }
    }
    & go run -mod=readonly ./examples/fullstack
    if ($LASTEXITCODE -ne 0) { throw 'Simulated fullstack demo failed' }
    Write-Output "Checked $($files.Count) Markdown files, $($programs.Count) standalone programs, example builds, and simulated fullstack run. Temporary builds: $scratch"
} finally {
    Pop-Location
}