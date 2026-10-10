#!/bin/sh
# esp-select.sh - which ESP /init reads this host's persisted config,
# authorized_keys and rescue host key from (issue #24, mirrored boot).
# Sourced by /init; never exits, never dies: when it cannot decide safely
# it says why and selects NOTHING - /init then boots with defaults/cmdline
# only and rescue SSH unconfigured, exactly what it did before for an
# ambiguous ESP. The pool boot never depends on this.
#
# The same rules as internal/bootenv.SelectHost (the Go tool); README
# "Mirrored boot: how members are recognised" is the user-facing version.
# Order, strongest first:
#
#   1. alpine-zfsboot.esp-uuids=A,B on the kernel command line: those
#      ESPs, nothing else. Best = highest MEMBER generation, then list
#      order. The installation is that of the first listed marked ESP; a
#      listed ESP marked for another installation is skipped.
#   2. alpine-zfsboot.esp-self=<uuid> on the command line (the tool writes
#      it into each BIOS member's own CMDLINE): the ESP stage2 booted from.
#      Its config belongs to the kernel that is running - used even when a
#      sibling has a newer generation (a warning names the sibling).
#      UEFI cannot tell which ESP it booted from (one shared .EFI cmdline):
#      step 2 does not apply there.
#   3. MEMBER markers (EFI/ALPINE/MEMBER): the ESPs carrying one
#      installation id - if there is exactly one such installation, or
#      the booted ESP of step 2 names it. Best = highest generation, then
#      FAT UUID (never the device name: sda/sdb can swap). A list in the
#      best member's own config (alpine-zfsboot.esp-uuids=) then applies as
#      in 1 - from this installation's own ESP only, never from an
#      arbitrary one (a foreign stick's config could list itself).
#      Two installations and nothing to choose: refuse.
#   4. No marker anywhere (every install before 0.5.0): exactly the old
#      rule - one qualifying LABEL=EFI ESP; several: refuse, naming them
#      and the ways out (esp adopt, alpine-zfsboot.esp-uuids=).
#
# In 1 and 3 the chosen ESP must carry the old markers too (a boot binary
# or KERNEL, and config/authorized_keys/host key) and, when it has an
# EFI/ALPINE/CHECKSUM, its KERNEL/INITRD must match it - otherwise the next
# best member is used, with a warning. A FAT UUID on two devices (a dd
# clone, an md member) anywhere in the candidate set: refuse.
#
# A marker counts only when it decodes and its ESP_UUID is the ESP's own
# FAT UUID (a marker copied onto another ESP does not make it a member).
#
# Inputs: ESP_SELECT_CMDLINE_LIST, ESP_SELECT_SELF (raw, from the
# cmdline), and the commands blkid/mount/umount (stubbed by
# tests/run-tests.sh). Outputs: ESP_SELECTED_DEV, ESP_SELECTED_UUID,
# ESP_SELECTED_INSTALL_ID (may be empty), ESP_SELECTED_REASON; return 0
# when an ESP was selected.

ESP_MNT="${ROOTFS:-}/tmp/esp-config"
ESP_SCAN="${ROOTFS:-}/tmp/esp-scan"
ESP_MEMBER_HEADER="alpine-zfsboot esp-member 1"
ESP_SUM_HEADER="alpine-zfsboot payload-sum 1"

# _esp_norm_uuid STRING - upper-cased FAT UUID (XXXX-XXXX), or return 1.
_esp_norm_uuid() {
    _u="$(printf '%s' "$1" | tr 'a-f' 'A-F')"
    case "$_u" in
        [0-9A-F][0-9A-F][0-9A-F][0-9A-F]-[0-9A-F][0-9A-F][0-9A-F][0-9A-F]) printf '%s' "$_u" ;;
        *) return 1 ;;
    esac
}

# _esp_norm_list LIST - normalized "A B C" from "a,B,c"; return 1 on an
# empty, malformed or repeated entry.
_esp_norm_list() {
    _out=""
    _rest="$1,"
    while [ -n "$_rest" ]; do
        _item="${_rest%%,*}"
        _rest="${_rest#*,}"
        _n="$(_esp_norm_uuid "$_item")" || return 1
        case " $_out " in *" $_n "*) return 1 ;; esac
        _out="${_out:+$_out }$_n"
    done
    [ -n "$_out" ] || return 1
    printf '%s' "$_out"
}

# _esp_read_member DIR - prints "id gen espuuid members(comma)" for a
# MEMBER file that decodes, returns 1 for an unreadable one, 2 for none.
_esp_read_member() {
    _f="$1/EFI/ALPINE/MEMBER"
    [ -e "$_f" ] || return 2
    _id="" _gen="" _eu="" _mem="" _first=1 _bad=0
    while IFS= read -r _l || [ -n "$_l" ]; do
        _l="${_l%$(printf '\r')}"
        if [ "$_first" = 1 ]; then
            [ "$_l" = "$ESP_MEMBER_HEADER" ] || _bad=1
            _first=0
            continue
        fi
        case "$_l" in
            INSTALL_ID=*) _id="${_l#INSTALL_ID=}" ;;
            GENERATION=*) _gen="${_l#GENERATION=}" ;;
            ESP_UUID=*) _eu="${_l#ESP_UUID=}" ;;
            MEMBERS=*) _mem="${_l#MEMBERS=}" ;;
        esac
    done < "$_f"
    [ "$_bad" = 0 ] || return 1
    case "$_id" in
        [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
        *) return 1 ;;
    esac
    case "$_gen" in ''|*[!0-9]*) return 1 ;; esac
    [ "${#_gen}" -le 18 ] || return 1
    _gen="$(printf '%s' "$_gen" | sed 's/^0*\([0-9]\)/\1/')"
    case "$_eu" in
        [0-9A-F][0-9A-F][0-9A-F][0-9A-F]-[0-9A-F][0-9A-F][0-9A-F][0-9A-F]) ;;
        *) return 1 ;;
    esac
    if [ -n "$_mem" ]; then
        _esp_norm_list "$_mem" >/dev/null || return 1
    fi
    printf '%s %s %s %s\n' "$_id" "$_gen" "$_eu" "${_mem:--}"
}

# _esp_scan - mounts every vfat volume blkid reports, read-only, one at a
# time, and records one line per volume in $ESP_SCAN:
#   dev uuid label alpine qual mstate id gen members cfglist
# mstate: none | valid | copied | bad. "-" for empty fields.
_esp_scan() {
    mkdir -p "$ESP_MNT"
    : > "$ESP_SCAN"
    blkid 2>/dev/null | while IFS= read -r _line; do
        case "$_line" in *' TYPE="vfat"'*) ;; *) continue ;; esac
        _dev="${_line%%:*}"
        _uuid=""
        case "$_line" in *' UUID="'*)
            _uuid="${_line#*' UUID="'}"; _uuid="${_uuid%%\"*}" ;;
        esac
        _label="-"
        case "$_line" in *' LABEL="'*)
            _label="${_line#*' LABEL="'}"; _label="${_label%%\"*}" ;;
        esac
        _uuid="$(_esp_norm_uuid "$_uuid")" || continue
        mount -t vfat -o ro "$_dev" "$ESP_MNT" 2>/dev/null || {
            printf '%s %s %s 0 0 none - - - -\n' "$_dev" "$_uuid" "${_label:--}" >> "$ESP_SCAN"
            continue
        }
        _alp=0; [ -d "$ESP_MNT/EFI/ALPINE" ] && _alp=1
        _mk=0
        [ -r "$ESP_MNT/EFI/ALPINE/config" ] && _mk=1
        [ -r "$ESP_MNT/EFI/ALPINE/authorized_keys" ] && _mk=1
        [ -r "$ESP_MNT/EFI/ALPINE/ssh_host_ed25519_key" ] && _mk=1
        _q=0
        if [ "$_mk" = 1 ] && { [ -f "$ESP_MNT/EFI/BOOT/BOOTX64.EFI" ] || [ -f "$ESP_MNT/EFI/BOOT/BOOTAA64.EFI" ] || \
                               [ -f "$ESP_MNT/EFI/ALPINE/KERNEL" ]; }; then
            _q=1
        fi
        _ms=none _mid=- _mgen=- _mmem=-
        _m="$(_esp_read_member "$ESP_MNT")"
        case $? in
            0)
                set -- $_m
                _mid="$1" _mgen="$2" _mmem="$4"
                if [ "$3" = "$_uuid" ]; then _ms=valid; else _ms=copied; fi
                ;;
            1) _ms=bad ;;
        esac
        _cl="$(sed -n 's/^alpine-zfsboot\.esp-uuids=//p' "$ESP_MNT/EFI/ALPINE/config" 2>/dev/null | tail -n 1 | tr -d '\r ')"
        umount "$ESP_MNT" 2>/dev/null
        printf '%s %s %s %s %s %s %s %s %s %s\n' "$_dev" "$_uuid" "$(printf '%s' "$_label" | tr ' ' '_')" "$_alp" "$_q" \
            "$_ms" "$_mid" "$_mgen" "$_mmem" "${_cl:--}" >> "$ESP_SCAN"
    done
}

# _esp_field UUID N - field N of the scan row of UUID (first row).
_esp_field() {
    awk -v u="$1" -v n="$2" '$2==u {print $n; exit}' "$ESP_SCAN"
}

_esp_count() {
    awk -v u="$1" '$2==u {c++} END {print c+0}' "$ESP_SCAN"
}

_esp_devs() {
    awk -v u="$1" '$2==u {printf "%s%s", s, $1; s=", "}' "$ESP_SCAN"
}

# _esp_payload_ok UUID - mounts it and checks KERNEL/INITRD against its
# CHECKSUM (format 1 only; another format, a missing file or no sha256sum
# counts as "nothing to check", never as a mismatch).
_esp_payload_ok() {
    _pu="$1"
    _dev="$(_esp_field "$1" 1)"
    mount -t vfat -o ro "$_dev" "$ESP_MNT" 2>/dev/null || return 1
    _ok=0
    _sum="$ESP_MNT/EFI/ALPINE/CHECKSUM"
    if [ -r "$_sum" ] && [ "$(head -n 1 "$_sum" | tr -d '\r')" = "$ESP_SUM_HEADER" ] && command -v sha256sum >/dev/null 2>&1; then
        for _what in KERNEL INITRD; do
            set -- $(sed -n "s/^$_what //p" "$_sum" | tr -d '\r')
            [ $# -eq 3 ] || continue
            _f="$ESP_MNT/EFI/ALPINE/$_what"
            if [ ! -r "$_f" ] || [ "$(wc -c < "$_f" | tr -d ' ')" != "$1" ] || \
               [ "$(tail -c +$(($2 + 1)) "$_f" | sha256sum | cut -d' ' -f1)" != "$3" ]; then
                msg "WARNING: ESP $_pu payload check: $_what on $_dev does not match its CHECKSUM"
                _ok=1
            fi
        done
    fi
    umount "$ESP_MNT" 2>/dev/null
    return "$_ok"
}

_esp_choose() {
    ESP_SELECTED_DEV="$(_esp_field "$1" 1)"
    ESP_SELECTED_UUID="$1"
    ESP_SELECTED_INSTALL_ID=""
    [ "$(_esp_field "$1" 6)" = valid ] && ESP_SELECTED_INSTALL_ID="$(_esp_field "$1" 7)"
    ESP_SELECTED_REASON="$2"
}

# _esp_warn_set ID SELECTED_UUID EXPECTED_UUIDS - stale and missing
# members of installation ID, relative to the selected ESP.
_esp_warn_set() {
    _sg="$(_esp_field "$2" 8)"
    [ "$_sg" = - ] && _sg=0
    if [ -n "$1" ]; then
        awk -v id="$1" '$6=="valid" && $7==id {print $2, $8, $1}' "$ESP_SCAN" | while read -r _u _g _d; do
            [ "$_u" = "$2" ] && continue
            if [ "$_g" -gt "$_sg" ]; then
                msg "WARNING: ESP $_u ($_d) has a NEWER generation ($_g) than ESP $2 config is taken from ($_sg) - run 'alpine-zfsboot verify'"
            elif [ "$_g" -lt "$_sg" ]; then
                msg "WARNING: ESP $_u ($_d) is STALE (generation $_g < $_sg) - run 'alpine-zfsboot update'"
            fi
        done
    fi
    for _u in $3; do
        [ "$(_esp_count "$_u")" -eq 0 ] && msg "WARNING: ESP $_u of this host's set is not present (a dead or detached disk?) - booting without it"
    done
}

# _esp_select_list "A B" SOURCE ID - list mode (rules 1, and 3's config list).
_esp_select_list() {
    _list="$1"
    for _u in $_list; do
        if [ "$(_esp_count "$_u")" -gt 1 ]; then
            msg "WARNING: ESP UUID $_u (listed in $2) is on more than one device ($(_esp_devs "$_u")) - a cloned disk? Refusing to guess: applying NO ESP config"
            return 1
        fi
    done
    # identity: the given one, else that of the FIRST listed (present,
    # marked) ESP - the operator's order decides, never a generation
    # number another installation's disk could carry.
    _lid="$3"
    if [ -z "$_lid" ]; then
        for _u in $_list; do
            [ "$(_esp_count "$_u")" -eq 1 ] || continue
            if [ "$(_esp_field "$_u" 6)" = valid ]; then _lid="$(_esp_field "$_u" 7)"; break; fi
        done
    fi
    awk '$4=="1" {print $2, $1}' "$ESP_SCAN" | while read -r _u _d; do
        case " $_list " in *" $_u "*) ;; *) msg "ignoring ESP $_u ($_d): not in alpine-zfsboot.esp-uuids ($2)" ;; esac
    done
    for _u in $(_esp_order_list "$_list"); do
        _d="$(_esp_field "$_u" 1)"
        if [ "$(_esp_field "$_u" 6)" = valid ] && [ -n "$_lid" ] && [ "$(_esp_field "$_u" 7)" != "$_lid" ]; then
            msg "WARNING: listed ESP $_u ($_d) carries the marker of ANOTHER installation ($(_esp_field "$_u" 7)) - not used"
            continue
        fi
        if [ "$(_esp_field "$_u" 5)" != 1 ]; then
            msg "WARNING: listed ESP $_u ($_d) has no usable alpine-zfsboot config/boot files (not mountable, or empty) - trying the next one"
            continue
        fi
        if ! _esp_payload_ok "$_u"; then
            msg "WARNING: listed ESP $_u ($_d) failed its payload check - trying the next one"
            continue
        fi
        _esp_choose "$_u" "listed in alpine-zfsboot.esp-uuids from $2"
        _esp_warn_set "$_lid" "$_u" "$_list"
        return 0
    done
    msg "WARNING: none of the ESPs listed in alpine-zfsboot.esp-uuids ($2: $(echo $_list | tr ' ' ',')) is present and usable - applying NO ESP config (defaults/cmdline only, rescue SSH unconfigured)"
    return 1
}

# _esp_order_list "A B" - the present ones, best first: highest valid
# generation, then list order.
_esp_order_list() {
    _i=0
    for _u in $1; do
        _i=$((_i + 1))
        [ "$(_esp_count "$_u")" -eq 1 ] || continue
        _g="$(_esp_field "$_u" 8)"
        [ "$(_esp_field "$_u" 6)" = valid ] || _g=0
        printf '%s %s %s\n' "$_g" "$_i" "$_u"
    done | sort -k1,1nr -k2,2n | awk '{print $3}'
}

esp_select() {
    ESP_SELECTED_DEV="" ESP_SELECTED_UUID="" ESP_SELECTED_INSTALL_ID="" ESP_SELECTED_REASON=""
    _esp_scan

    # 1. explicit list on the command line
    if [ -n "${ESP_SELECT_CMDLINE_LIST:-}" ]; then
        if ! _l="$(_esp_norm_list "$ESP_SELECT_CMDLINE_LIST")"; then
            msg "WARNING: alpine-zfsboot.esp-uuids=$ESP_SELECT_CMDLINE_LIST on the kernel command line is malformed (want comma-separated FAT UUIDs like 6AC5-0B94, each once) - applying NO ESP config (defaults/cmdline only, rescue SSH unconfigured)"
            return 1
        fi
        _esp_select_list "$_l" "the kernel command line" ""
        return $?
    fi

    # 2. the ESP this boot came from (BIOS: alpine-zfsboot.esp-self=)
    _gid=""
    if [ -n "${ESP_SELECT_SELF:-}" ]; then
        if _s="$(_esp_norm_uuid "$ESP_SELECT_SELF")"; then
            case "$(_esp_count "$_s")" in
                1)
                    if [ "$(_esp_field "$_s" 5)" = 1 ] && [ "$(_esp_field "$_s" 6)" != copied ] && [ "$(_esp_field "$_s" 6)" != bad ]; then
                        _esp_choose "$_s" "the ESP this boot came from"
                        if [ "$(_esp_field "$_s" 6)" = valid ]; then
                            _esp_warn_set "$(_esp_field "$_s" 7)" "$_s" "$(_esp_field "$_s" 9 | tr ',' ' ' | sed 's/^-$//')"
                        fi
                        return 0
                    fi
                    msg "WARNING: the ESP this boot came from ($_s, $(_esp_field "$_s" 1)) has no usable alpine-zfsboot config - looking for its siblings"
                    [ "$(_esp_field "$_s" 6)" = valid ] && _gid="$(_esp_field "$_s" 7)"
                    ;;
                0) msg "WARNING: the ESP this boot came from ($_s) is not visible to Linux - looking for its siblings" ;;
                *)
                    msg "WARNING: ESP UUID $_s (the ESP this boot came from) is on more than one device ($(_esp_devs "$_s")) - a cloned disk? Refusing to guess: applying NO ESP config"
                    return 1
                    ;;
            esac
        else
            msg "WARNING: alpine-zfsboot.esp-self=$ESP_SELECT_SELF is not a FAT UUID - ignored"
        fi
    fi

    # 3. identity markers
    for _u in $(awk '$6=="copied" {print $2}' "$ESP_SCAN"); do
        msg "ignoring ESP $_u ($(_esp_field "$_u" 1)): its MEMBER marker was copied from another ESP - add it with 'alpine-zfsboot esp add $_u'"
    done
    for _u in $(awk '$6=="bad" {print $2}' "$ESP_SCAN"); do
        msg "ignoring ESP $_u ($(_esp_field "$_u" 1)): its MEMBER marker is unreadable or of an unknown format"
    done
    _ids="$(awk '$6=="valid" {print $7}' "$ESP_SCAN" | sort -u)"
    if [ -z "$_gid" ] && [ -n "$_ids" ]; then
        if [ "$(printf '%s\n' "$_ids" | wc -l)" -gt 1 ]; then
            msg "WARNING: ESPs of more than one alpine-zfsboot installation are present ($(awk '$6=="valid" {printf "%s%s=%s", s, $2, $7; s=", "}' "$ESP_SCAN")) and nothing says which one is this host's - applying NO ESP config (defaults/cmdline only, rescue SSH unconfigured). Set alpine-zfsboot.esp-uuids=<this host's ESP UUIDs> on the command line, or remove the other installation's marker with 'alpine-zfsboot esp remove <uuid>'"
            return 1
        fi
        _gid="$_ids"
    fi
    if [ -n "$_gid" ]; then
        _cands="$(awk -v id="$_gid" '$6=="valid" && $7==id {print $8, $2}' "$ESP_SCAN" | sort -k1,1nr -k2,2 | awk '{print $2}')"
        for _u in $_cands; do
            if [ "$(_esp_count "$_u")" -gt 1 ]; then
                msg "WARNING: ESP UUID $_u is on more than one device ($(_esp_devs "$_u")) - a cloned disk? Refusing to guess: applying NO ESP config"
                return 1
            fi
        done
        for _u in $(awk -v id="$_gid" '$6=="valid" && $7!=id {print $2}' "$ESP_SCAN"); do
            msg "ignoring ESP $_u ($(_esp_field "$_u" 1)): it belongs to another installation ($(_esp_field "$_u" 7))"
        done
        for _u in $(awk '$4=="1" && $6=="none" {print $2}' "$ESP_SCAN"); do
            msg "ignoring ESP $_u ($(_esp_field "$_u" 1)): alpine-zfsboot files but no MEMBER marker ('alpine-zfsboot esp adopt $_u --yes' makes it a member)"
        done
        _best="$(printf '%s\n' $_cands | head -n 1)"
        _cl="$(_esp_field "$_best" 10)"
        if [ -n "$_cl" ] && [ "$_cl" != - ]; then
            if _l="$(_esp_norm_list "$_cl")"; then
                _esp_select_list "$_l" "EFI/ALPINE/config of ESP $_best" "$_gid"
                return $?
            fi
            msg "WARNING: alpine-zfsboot.esp-uuids=$_cl in the config of ESP $_best is malformed - ignored, using the MEMBER markers"
        fi
        _exp="$(_esp_field "$_best" 9 | tr ',' ' ' | sed 's/^-$//')"
        for _u in $_cands; do
            _d="$(_esp_field "$_u" 1)"
            if [ "$(_esp_field "$_u" 5)" != 1 ]; then
                msg "WARNING: ESP $_u ($_d) has no usable alpine-zfsboot config/boot files - trying the next member"
                continue
            fi
            if ! _esp_payload_ok "$_u"; then
                msg "WARNING: ESP $_u ($_d) failed its payload check - trying the next member"
                continue
            fi
            _esp_choose "$_u" "member of installation $_gid, generation $(_esp_field "$_u" 8)"
            _esp_warn_set "$_gid" "$_u" "$_exp"
            return 0
        done
        msg "WARNING: no member of installation $_gid is usable - applying NO ESP config (defaults/cmdline only, rescue SSH unconfigured)"
        return 1
    fi

    # 4. no markers anywhere: the pre-0.5.0 rule, unchanged
    _leg="$(awk '$3=="EFI" && $5=="1" && $6=="none" {print $2}' "$ESP_SCAN" | sort)"
    _n="$(printf '%s' "$_leg" | grep -c .)"
    if [ "$_n" -eq 0 ]; then
        msg "no alpine-zfsboot ESP found (checked every LABEL=EFI device) - defaults/cmdline only, rescue SSH unconfigured"
        return 1
    fi
    for _u in $_leg; do
        if [ "$(_esp_count "$_u")" -gt 1 ]; then
            msg "WARNING: ESP UUID $_u is on more than one device ($(_esp_devs "$_u")) - a cloned disk? Refusing to guess: applying NO ESP config"
            return 1
        fi
    done
    if [ "$_n" -eq 1 ]; then
        _cl="$(_esp_field "$_leg" 10)"
        if [ -n "$_cl" ] && [ "$_cl" != - ] && _l="$(_esp_norm_list "$_cl")"; then
            _esp_select_list "$_l" "EFI/ALPINE/config of ESP $_leg" ""
            return $?
        fi
        _esp_choose "$_leg" "the only alpine-zfsboot ESP"
        return 0
    fi
    # several unmarked ESPs: only an identical list in all their configs decides
    _lists="$(for _u in $_leg; do _esp_field "$_u" 10; done | grep -v '^-$' | sort -u)"
    if [ -n "$_lists" ] && [ "$(printf '%s\n' "$_lists" | wc -l)" -eq 1 ] && _l="$(_esp_norm_list "$_lists")"; then
        _esp_select_list "$_l" "EFI/ALPINE/config of the unmarked ESPs" ""
        return $?
    fi
    _desc="$(for _u in $_leg; do printf '%s (UUID %s) ' "$(_esp_field "$_u" 1)" "$_u"; done)"
    msg "WARNING: $_n devices carry both an alpine-zfsboot boot binary and config/authorized_keys/host-key material: ${_desc}- ambiguous, applying NONE of it. This boot uses defaults/cmdline only, rescue SSH unconfigured. If they are this host's mirrored ESPs: 'alpine-zfsboot esp adopt $(echo $_leg) --yes', or set alpine-zfsboot.esp-uuids=$(echo $_leg | tr ' ' ',')"
    return 1
}

# esp_check_pool_identity POOL - after the pool is imported: the pool's
# org.alpinezfsboot:install-id (set by the tool, best-effort) against the
# installation the config came from. A warning only; never changes
# anything (the config is long applied by then).
esp_check_pool_identity() {
    [ -n "${ALPINE_ZFSBOOT_ESP_INSTALL_ID:-}" ] || return 0
    _pid="$(zfs get -H -o value org.alpinezfsboot:install-id "$1" 2>/dev/null)"
    case "$_pid" in
        ''|-) return 0 ;;
    esac
    if [ "$_pid" != "$ALPINE_ZFSBOOT_ESP_INSTALL_ID" ]; then
        msg "WARNING: pool $1 belongs to alpine-zfsboot installation $_pid, but the ESP config/keys were taken from installation $ALPINE_ZFSBOOT_ESP_INSTALL_ID (ESP ${ALPINE_ZFSBOOT_ESP_UUID:-?}) - an ESP from another machine? Check 'alpine-zfsboot status'"
    fi
}
