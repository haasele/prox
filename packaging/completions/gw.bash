# bash completion for gw                                   -*- shell-script -*-

__gw_debug()
{
    if [[ -n ${BASH_COMP_DEBUG_FILE:-} ]]; then
        echo "$*" >> "${BASH_COMP_DEBUG_FILE}"
    fi
}

# Homebrew on Macs have version 1.3 of bash-completion which doesn't include
# _init_completion. This is a very minimal version of that function.
__gw_init_completion()
{
    COMPREPLY=()
    _get_comp_words_by_ref "$@" cur prev words cword
}

__gw_index_of_word()
{
    local w word=$1
    shift
    index=0
    for w in "$@"; do
        [[ $w = "$word" ]] && return
        index=$((index+1))
    done
    index=-1
}

__gw_contains_word()
{
    local w word=$1; shift
    for w in "$@"; do
        [[ $w = "$word" ]] && return
    done
    return 1
}

__gw_handle_go_custom_completion()
{
    __gw_debug "${FUNCNAME[0]}: cur is ${cur}, words[*] is ${words[*]}, #words[@] is ${#words[@]}"

    local shellCompDirectiveError=1
    local shellCompDirectiveNoSpace=2
    local shellCompDirectiveNoFileComp=4
    local shellCompDirectiveFilterFileExt=8
    local shellCompDirectiveFilterDirs=16

    local out requestComp lastParam lastChar comp directive args

    # Prepare the command to request completions for the program.
    # Calling ${words[0]} instead of directly gw allows handling aliases
    args=("${words[@]:1}")
    # Disable ActiveHelp which is not supported for bash completion v1
    requestComp="GW_ACTIVE_HELP=0 ${words[0]} __completeNoDesc ${args[*]}"

    lastParam=${words[$((${#words[@]}-1))]}
    lastChar=${lastParam:$((${#lastParam}-1)):1}
    __gw_debug "${FUNCNAME[0]}: lastParam ${lastParam}, lastChar ${lastChar}"

    if [ -z "${cur}" ] && [ "${lastChar}" != "=" ]; then
        # If the last parameter is complete (there is a space following it)
        # We add an extra empty parameter so we can indicate this to the go method.
        __gw_debug "${FUNCNAME[0]}: Adding extra empty parameter"
        requestComp="${requestComp} \"\""
    fi

    __gw_debug "${FUNCNAME[0]}: calling ${requestComp}"
    # Use eval to handle any environment variables and such
    out=$(eval "${requestComp}" 2>/dev/null)

    # Extract the directive integer at the very end of the output following a colon (:)
    directive=${out##*:}
    # Remove the directive
    out=${out%:*}
    if [ "${directive}" = "${out}" ]; then
        # There is not directive specified
        directive=0
    fi
    __gw_debug "${FUNCNAME[0]}: the completion directive is: ${directive}"
    __gw_debug "${FUNCNAME[0]}: the completions are: ${out}"

    if [ $((directive & shellCompDirectiveError)) -ne 0 ]; then
        # Error code.  No completion.
        __gw_debug "${FUNCNAME[0]}: received error from custom completion go code"
        return
    else
        if [ $((directive & shellCompDirectiveNoSpace)) -ne 0 ]; then
            if [[ $(type -t compopt) = "builtin" ]]; then
                __gw_debug "${FUNCNAME[0]}: activating no space"
                compopt -o nospace
            fi
        fi
        if [ $((directive & shellCompDirectiveNoFileComp)) -ne 0 ]; then
            if [[ $(type -t compopt) = "builtin" ]]; then
                __gw_debug "${FUNCNAME[0]}: activating no file completion"
                compopt +o default
            fi
        fi
    fi

    if [ $((directive & shellCompDirectiveFilterFileExt)) -ne 0 ]; then
        # File extension filtering
        local fullFilter filter filteringCmd
        # Do not use quotes around the $out variable or else newline
        # characters will be kept.
        for filter in ${out}; do
            fullFilter+="$filter|"
        done

        filteringCmd="_filedir $fullFilter"
        __gw_debug "File filtering command: $filteringCmd"
        $filteringCmd
    elif [ $((directive & shellCompDirectiveFilterDirs)) -ne 0 ]; then
        # File completion for directories only
        local subdir
        # Use printf to strip any trailing newline
        subdir=$(printf "%s" "${out}")
        if [ -n "$subdir" ]; then
            __gw_debug "Listing directories in $subdir"
            __gw_handle_subdirs_in_dir_flag "$subdir"
        else
            __gw_debug "Listing directories in ."
            _filedir -d
        fi
    else
        while IFS='' read -r comp; do
            COMPREPLY+=("$comp")
        done < <(compgen -W "${out}" -- "$cur")
    fi
}

__gw_handle_reply()
{
    __gw_debug "${FUNCNAME[0]}"
    local comp
    case $cur in
        -*)
            if [[ $(type -t compopt) = "builtin" ]]; then
                compopt -o nospace
            fi
            local allflags
            if [ ${#must_have_one_flag[@]} -ne 0 ]; then
                allflags=("${must_have_one_flag[@]}")
            else
                allflags=("${flags[*]} ${two_word_flags[*]}")
            fi
            while IFS='' read -r comp; do
                COMPREPLY+=("$comp")
            done < <(compgen -W "${allflags[*]}" -- "$cur")
            if [[ $(type -t compopt) = "builtin" ]]; then
                [[ "${COMPREPLY[0]}" == *= ]] || compopt +o nospace
            fi

            # complete after --flag=abc
            if [[ $cur == *=* ]]; then
                if [[ $(type -t compopt) = "builtin" ]]; then
                    compopt +o nospace
                fi

                local index flag
                flag="${cur%=*}"
                __gw_index_of_word "${flag}" "${flags_with_completion[@]}"
                COMPREPLY=()
                if [[ ${index} -ge 0 ]]; then
                    PREFIX=""
                    cur="${cur#*=}"
                    ${flags_completion[${index}]}
                    if [ -n "${ZSH_VERSION:-}" ]; then
                        # zsh completion needs --flag= prefix
                        eval "COMPREPLY=( \"\${COMPREPLY[@]/#/${flag}=}\" )"
                    fi
                fi
            fi

            if [[ -z "${flag_parsing_disabled}" ]]; then
                # If flag parsing is enabled, we have completed the flags and can return.
                # If flag parsing is disabled, we may not know all (or any) of the flags, so we fallthrough
                # to possibly call handle_go_custom_completion.
                return 0;
            fi
            ;;
    esac

    # check if we are handling a flag with special work handling
    local index
    __gw_index_of_word "${prev}" "${flags_with_completion[@]}"
    if [[ ${index} -ge 0 ]]; then
        ${flags_completion[${index}]}
        return
    fi

    # we are parsing a flag and don't have a special handler, no completion
    if [[ ${cur} != "${words[cword]}" ]]; then
        return
    fi

    local completions
    completions=("${commands[@]}")
    if [[ ${#must_have_one_noun[@]} -ne 0 ]]; then
        completions+=("${must_have_one_noun[@]}")
    elif [[ -n "${has_completion_function}" ]]; then
        # if a go completion function is provided, defer to that function
        __gw_handle_go_custom_completion
    fi
    if [[ ${#must_have_one_flag[@]} -ne 0 ]]; then
        completions+=("${must_have_one_flag[@]}")
    fi
    while IFS='' read -r comp; do
        COMPREPLY+=("$comp")
    done < <(compgen -W "${completions[*]}" -- "$cur")

    if [[ ${#COMPREPLY[@]} -eq 0 && ${#noun_aliases[@]} -gt 0 && ${#must_have_one_noun[@]} -ne 0 ]]; then
        while IFS='' read -r comp; do
            COMPREPLY+=("$comp")
        done < <(compgen -W "${noun_aliases[*]}" -- "$cur")
    fi

    if [[ ${#COMPREPLY[@]} -eq 0 ]]; then
        if declare -F __gw_custom_func >/dev/null; then
            # try command name qualified custom func
            __gw_custom_func
        else
            # otherwise fall back to unqualified for compatibility
            declare -F __custom_func >/dev/null && __custom_func
        fi
    fi

    # available in bash-completion >= 2, not always present on macOS
    if declare -F __ltrim_colon_completions >/dev/null; then
        __ltrim_colon_completions "$cur"
    fi

    # If there is only 1 completion and it is a flag with an = it will be completed
    # but we don't want a space after the =
    if [[ "${#COMPREPLY[@]}" -eq "1" ]] && [[ $(type -t compopt) = "builtin" ]] && [[ "${COMPREPLY[0]}" == --*= ]]; then
       compopt -o nospace
    fi
}

# The arguments should be in the form "ext1|ext2|extn"
__gw_handle_filename_extension_flag()
{
    local ext="$1"
    _filedir "@(${ext})"
}

__gw_handle_subdirs_in_dir_flag()
{
    local dir="$1"
    pushd "${dir}" >/dev/null 2>&1 && _filedir -d && popd >/dev/null 2>&1 || return
}

__gw_handle_flag()
{
    __gw_debug "${FUNCNAME[0]}: c is $c words[c] is ${words[c]}"

    # if a command required a flag, and we found it, unset must_have_one_flag()
    local flagname=${words[c]}
    local flagvalue=""
    # if the word contained an =
    if [[ ${words[c]} == *"="* ]]; then
        flagvalue=${flagname#*=} # take in as flagvalue after the =
        flagname=${flagname%=*} # strip everything after the =
        flagname="${flagname}=" # but put the = back
    fi
    __gw_debug "${FUNCNAME[0]}: looking for ${flagname}"
    if __gw_contains_word "${flagname}" "${must_have_one_flag[@]}"; then
        must_have_one_flag=()
    fi

    # if you set a flag which only applies to this command, don't show subcommands
    if __gw_contains_word "${flagname}" "${local_nonpersistent_flags[@]}"; then
      commands=()
    fi

    # keep flag value with flagname as flaghash
    # flaghash variable is an associative array which is only supported in bash > 3.
    if [[ -z "${BASH_VERSION:-}" || "${BASH_VERSINFO[0]:-}" -gt 3 ]]; then
        if [ -n "${flagvalue}" ] ; then
            flaghash[${flagname}]=${flagvalue}
        elif [ -n "${words[ $((c+1)) ]}" ] ; then
            flaghash[${flagname}]=${words[ $((c+1)) ]}
        else
            flaghash[${flagname}]="true" # pad "true" for bool flag
        fi
    fi

    # skip the argument to a two word flag
    if [[ ${words[c]} != *"="* ]] && __gw_contains_word "${words[c]}" "${two_word_flags[@]}"; then
        __gw_debug "${FUNCNAME[0]}: found a flag ${words[c]}, skip the next argument"
        c=$((c+1))
        # if we are looking for a flags value, don't show commands
        if [[ $c -eq $cword ]]; then
            commands=()
        fi
    fi

    c=$((c+1))

}

__gw_handle_noun()
{
    __gw_debug "${FUNCNAME[0]}: c is $c words[c] is ${words[c]}"

    if __gw_contains_word "${words[c]}" "${must_have_one_noun[@]}"; then
        must_have_one_noun=()
    elif __gw_contains_word "${words[c]}" "${noun_aliases[@]}"; then
        must_have_one_noun=()
    fi

    nouns+=("${words[c]}")
    c=$((c+1))
}

__gw_handle_command()
{
    __gw_debug "${FUNCNAME[0]}: c is $c words[c] is ${words[c]}"

    local next_command
    if [[ -n ${last_command} ]]; then
        next_command="_${last_command}_${words[c]//:/__}"
    else
        if [[ $c -eq 0 ]]; then
            next_command="_gw_root_command"
        else
            next_command="_${words[c]//:/__}"
        fi
    fi
    c=$((c+1))
    __gw_debug "${FUNCNAME[0]}: looking for ${next_command}"
    declare -F "$next_command" >/dev/null && $next_command
}

__gw_handle_word()
{
    if [[ $c -ge $cword ]]; then
        __gw_handle_reply
        return
    fi
    __gw_debug "${FUNCNAME[0]}: c is $c words[c] is ${words[c]}"
    if [[ "${words[c]}" == -* ]]; then
        __gw_handle_flag
    elif __gw_contains_word "${words[c]}" "${commands[@]}"; then
        __gw_handle_command
    elif [[ $c -eq 0 ]]; then
        __gw_handle_command
    elif __gw_contains_word "${words[c]}" "${command_aliases[@]}"; then
        # aliashash variable is an associative array which is only supported in bash > 3.
        if [[ -z "${BASH_VERSION:-}" || "${BASH_VERSINFO[0]:-}" -gt 3 ]]; then
            words[c]=${aliashash[${words[c]}]}
            __gw_handle_command
        else
            __gw_handle_noun
        fi
    else
        __gw_handle_noun
    fi
    __gw_handle_word
}

_gw_allow()
{
    last_command="gw_allow"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--container-port=")
    two_word_flags+=("--container-port")
    local_nonpersistent_flags+=("--container-port")
    local_nonpersistent_flags+=("--container-port=")
    flags+=("--gateway-port=")
    two_word_flags+=("--gateway-port")
    local_nonpersistent_flags+=("--gateway-port")
    local_nonpersistent_flags+=("--gateway-port=")
    flags+=("--no-redirect")
    local_nonpersistent_flags+=("--no-redirect")
    flags+=("--project=")
    two_word_flags+=("--project")
    local_nonpersistent_flags+=("--project")
    local_nonpersistent_flags+=("--project=")
    flags+=("--proto=")
    two_word_flags+=("--proto")
    local_nonpersistent_flags+=("--proto")
    local_nonpersistent_flags+=("--proto=")
    flags+=("--publish")
    local_nonpersistent_flags+=("--publish")
    flags+=("--resolve=")
    two_word_flags+=("--resolve")
    local_nonpersistent_flags+=("--resolve")
    local_nonpersistent_flags+=("--resolve=")
    flags+=("--source=")
    two_word_flags+=("--source")
    local_nonpersistent_flags+=("--source")
    local_nonpersistent_flags+=("--source=")
    flags+=("--target=")
    two_word_flags+=("--target")
    local_nonpersistent_flags+=("--target")
    local_nonpersistent_flags+=("--target=")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_ca_install()
{
    last_command="gw_ca_install"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_ca_show()
{
    last_command="gw_ca_show"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_ca_uninstall()
{
    last_command="gw_ca_uninstall"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_ca()
{
    last_command="gw_ca"

    command_aliases=()

    commands=()
    commands+=("install")
    commands+=("show")
    commands+=("uninstall")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_check()
{
    last_command="gw_check"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--json")
    flags+=("-j")
    local_nonpersistent_flags+=("--json")
    local_nonpersistent_flags+=("-j")
    flags+=("--project=")
    two_word_flags+=("--project")
    local_nonpersistent_flags+=("--project")
    local_nonpersistent_flags+=("--project=")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_completion_bash()
{
    last_command="gw_completion_bash"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--no-descriptions")
    local_nonpersistent_flags+=("--no-descriptions")

    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_completion_fish()
{
    last_command="gw_completion_fish"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--no-descriptions")
    local_nonpersistent_flags+=("--no-descriptions")

    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_completion_powershell()
{
    last_command="gw_completion_powershell"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--no-descriptions")
    local_nonpersistent_flags+=("--no-descriptions")

    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_completion_zsh()
{
    last_command="gw_completion_zsh"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--no-descriptions")
    local_nonpersistent_flags+=("--no-descriptions")

    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_completion()
{
    last_command="gw_completion"

    command_aliases=()

    commands=()
    commands+=("bash")
    commands+=("fish")
    commands+=("powershell")
    commands+=("zsh")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_completions_bash()
{
    last_command="gw_completions_bash"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--help")
    flags+=("-h")
    local_nonpersistent_flags+=("--help")
    local_nonpersistent_flags+=("-h")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_completions_fish()
{
    last_command="gw_completions_fish"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_completions_man()
{
    last_command="gw_completions_man"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_completions_zsh()
{
    last_command="gw_completions_zsh"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_completions()
{
    last_command="gw_completions"

    command_aliases=()

    commands=()
    commands+=("bash")
    commands+=("fish")
    commands+=("man")
    commands+=("zsh")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_deny()
{
    last_command="gw_deny"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--project=")
    two_word_flags+=("--project")
    local_nonpersistent_flags+=("--project")
    local_nonpersistent_flags+=("--project=")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_doctor()
{
    last_command="gw_doctor"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--fix")
    local_nonpersistent_flags+=("--fix")
    flags+=("--json")
    flags+=("-j")
    local_nonpersistent_flags+=("--json")
    local_nonpersistent_flags+=("-j")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_enforce_off()
{
    last_command="gw_enforce_off"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_enforce_on()
{
    last_command="gw_enforce_on"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_enforce_status()
{
    last_command="gw_enforce_status"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_enforce()
{
    last_command="gw_enforce"

    command_aliases=()

    commands=()
    commands+=("off")
    commands+=("on")
    commands+=("status")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_events()
{
    last_command="gw_events"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--follow")
    flags+=("-f")
    local_nonpersistent_flags+=("--follow")
    local_nonpersistent_flags+=("-f")
    flags+=("--json")
    flags+=("-j")
    local_nonpersistent_flags+=("--json")
    local_nonpersistent_flags+=("-j")
    flags+=("--limit=")
    two_word_flags+=("--limit")
    two_word_flags+=("-n")
    local_nonpersistent_flags+=("--limit")
    local_nonpersistent_flags+=("--limit=")
    local_nonpersistent_flags+=("-n")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_help()
{
    last_command="gw_help"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    has_completion_function=1
    noun_aliases=()
}

_gw_ls()
{
    last_command="gw_ls"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--json")
    flags+=("-j")
    local_nonpersistent_flags+=("--json")
    local_nonpersistent_flags+=("-j")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_policy_set()
{
    last_command="gw_policy_set"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_policy_show()
{
    last_command="gw_policy_show"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_policy()
{
    last_command="gw_policy"

    command_aliases=()

    commands=()
    commands+=("set")
    commands+=("show")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_rm()
{
    last_command="gw_rm"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_run()
{
    last_command="gw_run"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()

    flags+=("--dir=")
    two_word_flags+=("--dir")
    local_nonpersistent_flags+=("--dir")
    local_nonpersistent_flags+=("--dir=")
    flags+=("--name=")
    two_word_flags+=("--name")
    local_nonpersistent_flags+=("--name")
    local_nonpersistent_flags+=("--name=")
    flags+=("--port-flag=")
    two_word_flags+=("--port-flag")
    local_nonpersistent_flags+=("--port-flag")
    local_nonpersistent_flags+=("--port-flag=")
    flags+=("--resolve=")
    two_word_flags+=("--resolve")
    local_nonpersistent_flags+=("--resolve")
    local_nonpersistent_flags+=("--resolve=")

    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_version()
{
    last_command="gw_version"

    command_aliases=()

    commands=()

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

_gw_root_command()
{
    last_command="gw"

    command_aliases=()

    commands=()
    commands+=("allow")
    commands+=("ca")
    commands+=("check")
    commands+=("completion")
    commands+=("completions")
    commands+=("deny")
    commands+=("doctor")
    commands+=("enforce")
    commands+=("events")
    commands+=("help")
    commands+=("ls")
    commands+=("policy")
    commands+=("rm")
    commands+=("run")
    commands+=("version")

    flags=()
    two_word_flags=()
    local_nonpersistent_flags=()
    flags_with_completion=()
    flags_completion=()


    must_have_one_flag=()
    must_have_one_noun=()
    noun_aliases=()
}

__start_gw()
{
    local cur prev words cword split
    declare -A flaghash 2>/dev/null || :
    declare -A aliashash 2>/dev/null || :
    if declare -F _init_completion >/dev/null 2>&1; then
        _init_completion -s || return
    else
        __gw_init_completion -n "=" || return
    fi

    local c=0
    local flag_parsing_disabled=
    local flags=()
    local two_word_flags=()
    local local_nonpersistent_flags=()
    local flags_with_completion=()
    local flags_completion=()
    local commands=("gw")
    local command_aliases=()
    local must_have_one_flag=()
    local must_have_one_noun=()
    local has_completion_function=""
    local last_command=""
    local nouns=()
    local noun_aliases=()

    __gw_handle_word
}

if [[ $(type -t compopt) = "builtin" ]]; then
    complete -o default -F __start_gw gw
else
    complete -o default -o nospace -F __start_gw gw
fi

# ex: ts=4 sw=4 et filetype=sh
