package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/opod-io/opod/internal/control"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// cmdCompletion prints a shell completion script. Outputs are designed to
// be sourced directly:
//
//	source <(opod completion bash)
//	source <(opod completion zsh)
//	opod completion fish | source
//
// The scripts cover the static subcommand list and call back into
// `opod completion __models` / `__installed` / `__clients` for dynamic
// lists (catalog model IDs, installed model IDs, client IDs).
func cmdCompletion(args []string) {
	help := helpSpec{
		name:    "completion",
		summary: "print shell completion script (bash | zsh | fish)",
		usage:   "opod completion <bash | zsh | fish>",
		examples: []string{
			"# bash (add to ~/.bashrc):",
			"source <(opod completion bash)",
			"",
			"# zsh (add to ~/.zshrc):",
			"source <(opod completion zsh)",
			"",
			"# fish:",
			"opod completion fish | source",
		},
	}
	if len(args) == 0 || wantsHelp(args) {
		showHelp(help)
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "fish":
		fmt.Print(fishCompletion)
	case "__models":
		// Internal hook used by completion scripts. Prints one catalog ID per line.
		cfg := loadConfigOrExit()
		cat, err := models.LoadCatalog(cfg.CatalogDir)
		if err != nil {
			os.Exit(0)
		}
		for _, e := range cat {
			fmt.Println(e.ID)
		}
	case "__installed":
		// Installed model IDs only (for `model remove`).
		cfg := loadConfigOrExit()
		st, err := store.OpenSQLite(cfg.Storage.DSN)
		if err != nil {
			return
		}
		defer st.Close()
		rows, _ := st.Models().List(context.Background())
		for _, m := range rows {
			fmt.Println(m.CatalogID)
		}
	case "__clients":
		for _, c := range control.Clients() {
			fmt.Println(c.ID)
		}
	default:
		die("unsupported shell: %s (try bash, zsh, or fish)", args[0])
	}
}

// trim trailing whitespace from the embedded scripts so `source <(…)` works
// even when shells are picky about trailing newlines.
func init() {
	bashCompletion = strings.TrimSpace(bashCompletion) + "\n"
	zshCompletion = strings.TrimSpace(zshCompletion) + "\n"
	fishCompletion = strings.TrimSpace(fishCompletion) + "\n"
}

var bashCompletion = `
# bash completion for opod
_opod() {
    local cur prev words cword
    # Use bash-completion v2's helper when available; otherwise fall back to
    # raw COMP_* vars so the script still works on macOS default bash and
    # on minimal Linux containers.
    if declare -F _init_completion >/dev/null 2>&1; then
        _init_completion || return
    else
        COMPREPLY=()
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
        words=("${COMP_WORDS[@]}")
        cword=$COMP_CWORD
    fi

    local sub=${words[1]:-}
    local subsub=${words[2]:-}

    # Top-level subcommands
    local cmds="up down status join node model shard token usage audit config doctor update upgrade connect disconnect invite completion version help"

    if [[ $cword -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "$cmds" -- "$cur") )
        return 0
    fi

    case "$sub" in
        model)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "add ls list ps search info load unload remove rm" -- "$cur") )
                return 0
            fi
            case "$subsub" in
                add|info)
                    COMPREPLY=( $(compgen -W "$(opod completion __models 2>/dev/null)" -- "$cur") )
                    ;;
                load|unload|remove|rm)
                    COMPREPLY=( $(compgen -W "$(opod completion __installed 2>/dev/null)" -- "$cur") )
                    ;;
            esac
            return 0
            ;;
        connect|disconnect)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "$(opod completion __clients 2>/dev/null) --list" -- "$cur") )
            fi
            return 0
            ;;
        shard)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "create ls list remove rm" -- "$cur") )
                return 0
            fi
            case "$subsub" in
                create|remove|rm)
                    COMPREPLY=( $(compgen -W "$(opod completion __models 2>/dev/null)" -- "$cur") )
                    ;;
            esac
            return 0
            ;;
        node)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "ls list show drain remove rm" -- "$cur") )
            fi
            return 0
            ;;
        token)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "create ls list edit expire renew budget revoke" -- "$cur") )
            fi
            return 0
            ;;
        config)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "show path edit" -- "$cur") )
            fi
            return 0
            ;;
        completion)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "bash zsh fish" -- "$cur") )
            fi
            return 0
            ;;
    esac
}
complete -F _opod opod
`

var zshCompletion = `
#compdef opod
_opod() {
    local -a cmds
    cmds=(
        'up:start the local node'
        'down:stop the local node'
        'status:show cluster status'
        'join:join an existing cluster as a worker'
        'node:manage worker nodes'
        'model:install, list, search, inspect, or uninstall LLM models'
        'shard:orchestrate sharded models'
        'token:manage API keys'
        'usage:show recent inference usage'
        'audit:show recent admin audit log'
        'config:show / edit runtime config'
        'doctor:diagnose common problems'
        'update:check / install the latest release'
        'upgrade:alias for update'
        'connect:print copy-paste config for a tool'
        'disconnect:print reversal steps for a previous connect'
        'invite:create a user-scope token + share card'
        'completion:print shell completion script'
        'version:print version'
        'help:show help'
    )
    _arguments -C \
        '1: :->cmd' \
        '2: :->sub' \
        '*: :->args'

    case $state in
        cmd)
            _describe 'command' cmds
            ;;
        sub)
            case $words[2] in
                model)   _values 'subcommand' add ls list ps search info load unload remove rm ;;
                connect|disconnect)
                    local -a clients
                    clients=(${(f)"$(opod completion __clients 2>/dev/null)"})
                    _describe 'client' clients
                    ;;
                shard)   _values 'subcommand' create ls list remove rm ;;
                node)    _values 'subcommand' ls list show drain remove rm ;;
                token)   _values 'subcommand' create ls list edit expire renew budget revoke ;;
                config)  _values 'subcommand' show path edit ;;
                completion) _values 'shell' bash zsh fish ;;
            esac
            ;;
        args)
            case "$words[2] $words[3]" in
                'model add'|'model info')
                    local -a mods
                    mods=(${(f)"$(opod completion __models 2>/dev/null)"})
                    _describe 'model' mods
                    ;;
                'model load'|'model unload'|'model remove'|'model rm')
                    local -a mods
                    mods=(${(f)"$(opod completion __installed 2>/dev/null)"})
                    _describe 'installed model' mods
                    ;;
                'shard create'|'shard remove'|'shard rm')
                    local -a mods
                    mods=(${(f)"$(opod completion __models 2>/dev/null)"})
                    _describe 'model' mods
                    ;;
            esac
            ;;
    esac
}
compdef _opod opod
`

var fishCompletion = `
# fish completion for opod
function __opod_using_command
    set -l cmd (commandline -opc)
    test (count $cmd) -ge 2; and test $cmd[2] = $argv[1]
end

function __opod_using_subcommand
    set -l cmd (commandline -opc)
    test (count $cmd) -ge 3; and test "$cmd[2] $cmd[3]" = "$argv[1]"
end

complete -c opod -f

# Top-level
complete -c opod -n "not __fish_seen_subcommand_from up down status join node model shard token usage audit config doctor update upgrade connect disconnect invite completion version help" -a "up down status join node model shard token usage audit config doctor update upgrade connect disconnect invite completion version help"

# model subcommands
complete -c opod -n "__opod_using_command model" -a "add ls list ps search info load unload remove rm"
complete -c opod -n "__opod_using_subcommand 'model add'" -a "(opod completion __models 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'model info'" -a "(opod completion __models 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'model load'" -a "(opod completion __installed 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'model unload'" -a "(opod completion __installed 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'model remove'" -a "(opod completion __installed 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'model rm'" -a "(opod completion __installed 2>/dev/null)"

# connect
complete -c opod -n "__opod_using_command connect" -a "(opod completion __clients 2>/dev/null) --list"
complete -c opod -n "__opod_using_command disconnect" -a "(opod completion __clients 2>/dev/null)"

# shard subcommands
complete -c opod -n "__opod_using_command shard" -a "create ls list remove rm"
complete -c opod -n "__opod_using_subcommand 'shard create'" -a "(opod completion __models 2>/dev/null)"
complete -c opod -n "__opod_using_subcommand 'shard remove'" -a "(opod completion __models 2>/dev/null)"

# node / token / config
complete -c opod -n "__opod_using_command node" -a "ls list show drain remove rm"
complete -c opod -n "__opod_using_command token" -a "create ls list edit expire renew budget revoke"
complete -c opod -n "__opod_using_command config" -a "show path edit"
complete -c opod -n "__opod_using_command completion" -a "bash zsh fish"
`
