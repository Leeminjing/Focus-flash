$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$env:PSModulePath = [IO.Path]::Combine($PSHOME, 'Modules')
$source = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([Console]::In.ReadToEnd()))
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput($source, [ref]$tokens, [ref]$errors)
if ($errors.Count -ne 0) {
    '{"invalid":true}'
    exit
}

$locations = @('cd', 'chdir', 'sl', 'set-location', 'pushd', 'push-location', 'popd', 'pop-location')
$directories = @()
$changesPath = $false
foreach ($command in $ast.FindAll({param($node) $node -is [System.Management.Automation.Language.CommandAst]}, $true)) {
    $name = $command.GetCommandName()
    if ($null -eq $name) { continue }
    $name = ($name -split '[\\/]')[-1]
    if ($name -in $locations) {
        $nested = $false
        for ($parent = $command.Parent; $null -ne $parent; $parent = $parent.Parent) {
            if ($parent -is [System.Management.Automation.Language.SubExpressionAst]) {
                $nested = $true
                break
            }
        }
        $target = $null
        $valid = $name -notin @('popd', 'pop-location')
        $waiting = $false
        for ($i = 1; $i -lt $command.CommandElements.Count -and $valid; $i++) {
            $argument = $command.CommandElements[$i]
            if ($argument -is [System.Management.Automation.Language.CommandParameterAst]) {
                if ($waiting -or $target -ne $null -or $argument.ParameterName -notin @('Path', 'LiteralPath')) {
                    $valid = $false
                    continue
                }
                if ($null -eq $argument.Argument) {
                    $waiting = $true
                    continue
                }
                $argument = $argument.Argument
            }
            if ($target -ne $null -or $argument -isnot [System.Management.Automation.Language.StringConstantExpressionAst]) {
                $valid = $false
                continue
            }
            $target = $argument.Value
            $waiting = $false
        }
        $directories += @{target = $target; valid = $valid -and !$waiting; nested = $nested}
    }
    if ($name -in @('set-item', 'si', 'new-item', 'ni', 'remove-item', 'ri', 'clear-item', 'cli')) {
        foreach ($argument in $command.CommandElements) {
            if ($argument -is [System.Management.Automation.Language.CommandParameterAst]) {
                $argument = $argument.Argument
            }
            if ($argument -is [System.Management.Automation.Language.StringConstantExpressionAst] -and
                $argument.Value -match '^env:[\\/]?path$') {
                $changesPath = $true
            }
        }
    }
}

foreach ($assignment in $ast.FindAll({param($node) $node -is [System.Management.Automation.Language.AssignmentStatementAst]}, $true)) {
    if ($assignment.Left -is [System.Management.Automation.Language.VariableExpressionAst] -and
        $assignment.Left.VariablePath.UserPath -eq 'env:PATH') {
        $changesPath = $true
    }
}
foreach ($invoke in $ast.FindAll({param($node) $node -is [System.Management.Automation.Language.InvokeMemberExpressionAst]}, $true)) {
    if ($invoke.Static -and $invoke.Expression -is [System.Management.Automation.Language.TypeExpressionAst] -and
        $invoke.Expression.TypeName.FullName -in @('Environment', 'System.Environment') -and
        $invoke.Member.Value -eq 'SetEnvironmentVariable' -and $invoke.Arguments.Count -gt 0 -and
        $invoke.Arguments[0] -is [System.Management.Automation.Language.StringConstantExpressionAst] -and
        $invoke.Arguments[0].Value -eq 'PATH') {
        $changesPath = $true
    }
}

$values = @(
    foreach ($literal in $ast.FindAll({
        param($node)
        $node -is [System.Management.Automation.Language.StringConstantExpressionAst] -or
        $node -is [System.Management.Automation.Language.ExpandableStringExpressionAst]
    }, $true)) {
        $parent = $literal.Parent
        $target = $false
        if ($parent -is [System.Management.Automation.Language.CommandAst]) {
            $target = [object]::ReferenceEquals($parent.CommandElements[0], $literal)
        }
        @{text = $literal.Value; executable = $target}
    }
)
@{invalid = $false; values = $values; locations = $directories; changesPath = $changesPath} |
    Microsoft.PowerShell.Utility\ConvertTo-Json -Compress -Depth 4
