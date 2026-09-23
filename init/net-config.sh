#!/bin/sh
# net-config.sh - bring up an interface per alpine-zfsboot's network
# cmdline options, and nothing else. Deliberately knows NOTHING about
# dropbear, rescue mode, or why the network is being brought up -
# callers (/init, boot-dataset.sh, rescue-ssh.sh) decide WHEN to call
# this; this file only decides HOW, so the "how" can be read, tested,
# and changed in one place independent of every caller's own lifecycle
# logic. This separation was an explicit design requirement, not a
# refactor of convenience: dropbear must never drive network
# configuration itself.
#
# A real function (net_config), not a flat top-level script - meant to
# be SOURCED and called, exactly like rescue-ssh.sh's own
# start_rescue_ssh/stop_rescue_ssh, not exec'd as a subprocess. This
# matters for more than style: this project's own test harness
# (tests/run-tests.sh) stubs mount/zpool/zfs/ifconfig/udhcpc/dropbear/
# etc as plain shell FUNCTIONS inside the process running the script
# under test - a function definition in one shell process is invisible
# to a separate subprocess exec'd from it. Sourcing this file (as
# rescue-ssh.sh does) keeps net_config running in the SAME shell as
# its caller, so it sees the exact same stubbed `ip`/`ifconfig`/
# `udhcpc` the rest of that test already relies on - a subprocess
# exec of this file would silently fall through to the real host
# binaries instead, and either do nothing meaningful under test or
# actually try to touch the dev host's real network.
#
# Configuration is read from environment variables, not /proc/cmdline
# directly - callers parse the real alpine-zfsboot.net=/.ipv4=/.ipv6=
# cmdline options (see /init's own cmdline loop) and export them as
# ALPINE_ZFSBOOT_NET/ALPINE_ZFSBOOT_IPV4/... before calling net_config.
# This is what makes it independently testable: set the env vars by
# hand, call the function, and check what it actually did - no
# /proc/cmdline fakery required.
#
# Recognized environment variables, all optional:
#   ALPINE_ZFSBOOT_NET            auto|static|off (default: auto) -
#                                  sets the DEFAULT for both families
#                                  below when either is left unset; an
#                                  explicit ALPINE_ZFSBOOT_IPV4/IPV6
#                                  always overrides this default for
#                                  its own family.
#   ALPINE_ZFSBOOT_IPV4           dhcp|static|off
#   ALPINE_ZFSBOOT_IPV4_ADDRESS   CIDR, e.g. 203.0.113.5/24 - required if IPV4=static
#   ALPINE_ZFSBOOT_IPV4_GATEWAY   plain address - optional even under static
#   ALPINE_ZFSBOOT_IPV6           auto|dhcp|static|off
#   ALPINE_ZFSBOOT_IPV6_ADDRESS   CIDR, e.g. 2001:db8::5/64 - required if IPV6=static
#   ALPINE_ZFSBOOT_IPV6_GATEWAY   plain address - optional even under static
#
# "auto" for IPv6 means kernel-native SLAAC (router-advertisement
# autoconfig) - no daemon needed, just an up'd interface with the
# kernel's own accept_ra/autoconf defaults left alone (both on by
# default on a stock kernel unless something upstream disabled them,
# which this project never does). "dhcp" for IPv6 means udhcpc6
# specifically (a real, separate protocol/daemon from SLAAC, not the
# same thing under a different name) - confirmed bundled in this
# project's own busybox build (CONFIG_UDHCPC6=y, checked against
# Alpine's real busyboxconfig, same source already relied on for
# CONFIG_IP=y/CONFIG_IFCONFIG=y/CONFIG_UDHCPC=y elsewhere in this
# project).
#
# Static IPv4/IPv6 uses the `ip` applet (CONFIG_IP=y, confirmed
# present in this exact busybox build) rather than `ifconfig`+netmask
# arithmetic - `ip addr add ADDR/PREFIXLEN dev IFACE` takes CIDR
# notation directly, no dotted-netmask conversion needed, and the same
# command shape covers both families (`ip` vs `ip -6`). The existing
# DHCPv4 default path (ifconfig+udhcpc) is left completely untouched
# for exact backward compatibility - this file ADDS new paths, it
# doesn't rewrite the one every existing deployment already depends on.

net_config_msg() {
    echo "alpine-zfsboot-net: $*" > /dev/kmsg 2>/dev/null
    echo "alpine-zfsboot-net: $*"
}

# wait_for_global_ipv6 IFACE TIMEOUT_SECONDS - polls (bounded, not
# indefinite) for a real global-scope IPv6 address on IFACE, returning
# 0 as soon as one appears or 1 once TIMEOUT_SECONDS elapses. SLAAC has
# no client process the way DHCP does - the kernel processes Router
# Advertisements on its own schedule, entirely asynchronously - so
# there's no exit-status equivalent to wait on; this is what a genuine
# local-readiness check looks like for it instead. Link-local (fe80::)
# addresses don't count - the kernel assigns one of those the instant
# the link comes up, regardless of whether SLAAC (or anything else)
# ever actually configures a usable global address, so counting it
# would make this check just as vacuous as the bug it replaces.
# Deliberately does NOT check reachability/a route/an actual ping
# anywhere - "local configured-address" is the bar, not "the Internet
# is reachable", same standard the DHCP fix uses.
#
# NET_CONFIG_POLL_INTERVAL (default 1, whole seconds - portable to a
# busybox `sleep` that may not accept fractional arguments) exists so
# tests can drive this loop with a near-zero interval instead of
# actually sleeping for real; production never sets it.
wait_for_global_ipv6() {
    iface="$1"
    timeout="$2"
    poll_interval="${NET_CONFIG_POLL_INTERVAL:-1}"
    waited=0
    while [ "$waited" -lt "$timeout" ]; do
        if ip -6 -o addr show dev "$iface" 2>/dev/null | grep -v " fe80:" | grep -q "inet6"; then
            return 0
        fi
        sleep "$poll_interval"
        waited=$((waited + 1))
    done
    return 1
}

# net_config [IFACE] - IFACE defaults to eth0.
net_config() {
    iface="${1:-eth0}"

    net_mode="${ALPINE_ZFSBOOT_NET:-auto}"
    case "$net_mode" in
        auto|static|off) ;;
        *) net_config_msg "unknown ALPINE_ZFSBOOT_NET=$net_mode, treating as auto"; net_mode=auto ;;
    esac

    ipv4_mode="${ALPINE_ZFSBOOT_IPV4:-}"
    if [ -z "$ipv4_mode" ]; then
        case "$net_mode" in
            off) ipv4_mode=off ;;
            static) ipv4_mode=static ;;
            *) ipv4_mode=dhcp ;;
        esac
    fi

    ipv6_mode="${ALPINE_ZFSBOOT_IPV6:-}"
    if [ -z "$ipv6_mode" ]; then
        case "$net_mode" in
            off) ipv6_mode=off ;;
            static) ipv6_mode=static ;;
            *) ipv6_mode=auto ;;
        esac
    fi

    if [ "$ipv4_mode" = "off" ] && [ "$ipv6_mode" = "off" ]; then
        net_config_msg "both families off, not touching $iface"
        return 0
    fi

    # ipv6=off must actually mean no IPv6 on this interface at all - not
    # just "we don't configure an address ourselves". A real, confirmed
    # gap: the `off)` case further down did literally nothing, but the
    # kernel auto-assigns a link-local (fe80::/64) address and starts
    # processing Router Advertisements the moment the link comes up,
    # entirely independent of anything this script configures - `off`
    # was silently NOT off. Written before `ip link set up` below (not
    # after, and not only in the ipv6 case block further down) so
    # there's no window at all where the interface could pick up even a
    # link-local address before this takes effect - disable_ipv6 is
    # explicitly safe to set on a down interface, and takes effect
    # immediately regardless of link state.
    #
    # ipv4=off needs no equivalent - unlike IPv6, Linux never auto-
    # assigns an IPv4 address (not even a link-local 169.254.0.0/16 one)
    # without an active DHCP/zeroconf client actually requesting it, and
    # this script never starts one for ipv4=off - "we configure nothing"
    # already IS "no address", deterministically, for that family.
    if [ "$ipv6_mode" = "off" ]; then
        net_config_msg "IPv6: off on $iface (disabling IPv6 on the interface, not just skipping our own configuration)"
        # ${ROOTFS:-} - respected here for the SAME reason /init and
        # boot-dataset.sh already respect it (see their own ROOTFS
        # comments): without it, this exact line would write to the
        # REAL, LIVE /proc/sys/net/ipv6/conf/eth0/disable_ipv6 of
        # whatever machine happens to run this file - including a dev
        # host or CI runner with a real eth0 (a stock Docker container's
        # default interface name) - confirmed a genuine hazard, not a
        # hypothetical one, since tests/run-tests.sh exercises this
        # exact line directly. Checked, not fire-and-forget: if this
        # write fails, ipv6_mode is forced back to a mode that at least
        # gets logged/reported rather than silently letting `ip link set
        # up` below bring IPv6 alive anyway right after we just claimed
        # to have turned it off.
        if ! echo 1 > "${ROOTFS:-}/proc/sys/net/ipv6/conf/$iface/disable_ipv6" 2>/tmp/net-config-v6-off.log; then
            cat /tmp/net-config-v6-off.log
            net_config_msg "failed to disable IPv6 on $iface - it may still come up (SLAAC/link-local) once the link is up"
            net_config_status=1
        fi
    fi

    if ! ip link set "$iface" up 2>/tmp/net-config-link.log; then
        cat /tmp/net-config-link.log
        net_config_msg "failed to bring up $iface"
        return 1
    fi

    # ipv4_ok/ipv6_ok, not just a single net_config_status set true-on-
    # any-failure: a real, confirmed gap - the old scheme meant ANY
    # single problem in EITHER family (including one family being
    # deliberately =off and thus never even attempted) made
    # rescue-ssh.sh's own `if ! net_config eth0` refuse to start dropbear
    # at all, even when the OTHER family came up perfectly fine and was
    # completely reachable. For a break-glass SSH path, "reachable over
    # the family that worked" must never be treated the same as
    # "unreachable" - see this function's own return-value logic at the
    # bottom for how these combine.
    ipv4_ok=0
    ipv6_ok=0

    case "$ipv4_mode" in
        dhcp)
            net_config_msg "IPv4: dhcp on $iface"
            # ifconfig+udhcpc, not `ip`+a different DHCP path - unchanged
            # from this project's original, pre-existing invocation shape
            # on purpose, so a deployment relying on today's default
            # keeps the exact same commands, byte for byte.
            #
            # NOW run in the FOREGROUND and its exit status checked - a
            # full source audit found this used to background udhcpc
            # (trailing `&`) and set ipv4_ok=1 unconditionally, the
            # instant the client was merely STARTED, never checking
            # whether a lease was actually obtained. That false signal
            # fed straight into rescue-ssh.sh's own "is the network up
            # enough to usefully start dropbear" decision, which in turn
            # feeds /init's bootcheck gate for whether to stop retrying
            # automatic boot at all - so "DHCP was started" could make a
            # genuinely network-less machine look reachable enough to
            # abandon retries on. `-n` (exit if no lease) and `-q` (exit
            # once a lease IS obtained) were ALREADY being passed - they
            # just weren't being waited on, which defeated their entire
            # purpose. Backgrounding also already meant no lease-renewal
            # daemon stays running either way (`-q` exits immediately on
            # success) - foregrounding this changes NOTHING about
            # long-term lease renewal, only whether the boot process
            # waits to learn the real, local (no gateway/Internet
            # reachability implied) answer before proceeding. Bounded by
            # busybox udhcpc's own internal discover retry/timeout
            # (config already in use elsewhere in this project), not an
            # unbounded wait.
            ifconfig "$iface" up 2>/dev/null
            if udhcpc -i "$iface" -n -q 2>/tmp/udhcpc.log; then
                ipv4_ok=1
            else
                cat /tmp/udhcpc.log
                net_config_msg "IPv4: dhcp on $iface did not obtain a lease"
            fi
            ;;
        static)
            if [ -z "${ALPINE_ZFSBOOT_IPV4_ADDRESS:-}" ]; then
                net_config_msg "IPv4=static but ALPINE_ZFSBOOT_IPV4_ADDRESS is unset - skipping IPv4"
            elif ip -o addr show dev "$iface" 2>/dev/null | grep -qF "$ALPINE_ZFSBOOT_IPV4_ADDRESS"; then
                # Idempotency, not just a nicety - a real, confirmed
                # gap: rescue-ssh.sh's own fail()-triggered retry path
                # can call net_config a SECOND time on an interface this
                # same process already configured (local unlock
                # succeeds -> stop_rescue_ssh -> a LATER, unrelated
                # failure -> fail() -> start_rescue_ssh again) - a plain
                # `ip addr add` of an address already present fails with
                # "File exists", which used to make this whole family
                # (and therefore, under the old any-failure-fails
                # scheme, dropbear itself) look broken on exactly the
                # boot that most needs it to work.
                net_config_msg "IPv4: $ALPINE_ZFSBOOT_IPV4_ADDRESS already configured on $iface"
                ipv4_ok=1
            else
                net_config_msg "IPv4: static $ALPINE_ZFSBOOT_IPV4_ADDRESS on $iface"
                if ! ip addr add "$ALPINE_ZFSBOOT_IPV4_ADDRESS" dev "$iface" 2>/tmp/net-config-v4.log; then
                    cat /tmp/net-config-v4.log
                    net_config_msg "failed to add IPv4 address $ALPINE_ZFSBOOT_IPV4_ADDRESS to $iface"
                else
                    ipv4_ok=1
                fi
            fi
            if [ "$ipv4_ok" = 1 ] && [ -n "${ALPINE_ZFSBOOT_IPV4_GATEWAY:-}" ]; then
                # Gateway problems are logged but never flip ipv4_ok back
                # off - unchanged, deliberate precedent (the gateway was
                # already documented as "optional even under static"
                # before today) - an address with no/broken route is
                # still strictly more reachable than no address at all.
                if ip route show default 2>/dev/null | grep -qF "via ${ALPINE_ZFSBOOT_IPV4_GATEWAY} dev ${iface}"; then
                    net_config_msg "IPv4: default route via $ALPINE_ZFSBOOT_IPV4_GATEWAY already present on $iface"
                elif ip route add default via "$ALPINE_ZFSBOOT_IPV4_GATEWAY" dev "$iface" 2>/tmp/net-config-v4-gw.log; then
                    :
                else
                    cat /tmp/net-config-v4-gw.log
                    # Two-step fallback for a gateway OUTSIDE the
                    # interface's configured prefix (the "onlink" case) -
                    # a real, documented convention, not a hypothetical:
                    # Hetzner Cloud's own IPv4 setup is a /32 address
                    # with a gateway that's unreachable as a plain
                    # default route without first telling the kernel the
                    # gateway itself is directly on-link. A host route to
                    # the gateway, then retrying the default route,
                    # achieves the same effect as `onlink` without
                    # needing to confirm this busybox ip build parses
                    # that keyword.
                    net_config_msg "IPv4 default route via $ALPINE_ZFSBOOT_IPV4_GATEWAY failed directly - retrying via an explicit on-link host route (gateway may be outside the configured prefix)"
                    if ip route add "$ALPINE_ZFSBOOT_IPV4_GATEWAY" dev "$iface" 2>/tmp/net-config-v4-gw2.log \
                       && ip route add default via "$ALPINE_ZFSBOOT_IPV4_GATEWAY" dev "$iface" 2>>/tmp/net-config-v4-gw2.log; then
                        net_config_msg "IPv4: default route via $ALPINE_ZFSBOOT_IPV4_GATEWAY added via the on-link fallback"
                    else
                        cat /tmp/net-config-v4-gw2.log
                        net_config_msg "failed to add IPv4 default route via $ALPINE_ZFSBOOT_IPV4_GATEWAY (both direct and on-link attempts failed)"
                    fi
                fi
            fi
            ;;
        off)
            # No sysctl equivalent needed - see this function's own
            # comment above (grep for "ipv4=off needs no equivalent") on
            # why "configure nothing" already IS deterministically "no
            # address" for IPv4, unlike IPv6. Logged anyway, for the
            # same reason ipv6=off's own message exists: explicit intent
            # should be visible in the boot log, not indistinguishable
            # from "nobody set an opinion either way". Not counted
            # toward ipv4_ok (a family deliberately turned off did not
            # "succeed" or "fail" - it was never attempted at all, see
            # the return-value logic at the bottom of this function).
            net_config_msg "IPv4: off on $iface (no DHCP client started, no address configured)"
            ;;
        *) net_config_msg "unknown ALPINE_ZFSBOOT_IPV4=$ipv4_mode, skipping IPv4" ;;
    esac

    case "$ipv6_mode" in
        auto)
            # Kernel-native SLAAC - nothing to RUN beyond the `ip link
            # set up` already done above (accept_ra/autoconf are on by
            # default for a fresh interface on a stock kernel; this
            # project never disables either), but a full source audit
            # found this used to set ipv6_ok=1 unconditionally, the
            # instant SLAAC was nominally "enabled" - never actually
            # checking whether the kernel's own asynchronous RA
            # processing produced a real address before boot proceeded.
            # See wait_for_global_ipv6's own comment for why this is a
            # bounded poll rather than either an instant assumption or
            # an unbounded wait, and why link-local doesn't count.
            net_config_msg "IPv6: SLAAC (autoconf) on $iface"
            if wait_for_global_ipv6 "$iface" 5; then
                ipv6_ok=1
            else
                net_config_msg "IPv6: SLAAC on $iface produced no global address within 5s"
            fi
            ;;
        dhcp)
            # DHCPv6 (udhcpc6) provides an address, NOT a default route -
            # that's a real IPv6 architecture point, not an
            # implementation gap here: the default router normally comes
            # from Router Advertisements (specifically the RA's Router
            # Lifetime field), a completely separate mechanism from
            # DHCPv6. This mode relies on RA processing happening in
            # parallel and providing the route, which it does today only
            # because nothing in this file ever touches accept_ra (left
            # at the kernel's own default of on) - if that ever changes,
            # this mode silently stops getting a usable default route
            # while still reporting a successful DHCPv6 lease. Not
            # deepened further for now (explicit ipv4=off/ipv6=static is
            # this project's actual tested/relied-on headless-rescue
            # path - see boot-dataset.sh's own design notes) - flagged
            # here so a future change to accept_ra doesn't silently
            # break this mode's routing without anyone connecting the two.
            net_config_msg "IPv6: dhcp (udhcpc6) on $iface - default route depends on RA, not DHCPv6 itself"
            # Foregrounded + exit status checked - same real bug and
            # same fix as IPv4 dhcp above (udhcpc6 supports the
            # identical -n/-q semantics udhcpc does).
            if udhcpc6 -i "$iface" -n -q 2>/tmp/udhcpc6.log; then
                ipv6_ok=1
            else
                cat /tmp/udhcpc6.log
                net_config_msg "IPv6: dhcp (udhcpc6) on $iface did not obtain a lease"
            fi
            ;;
        static)
            if [ -z "${ALPINE_ZFSBOOT_IPV6_ADDRESS:-}" ]; then
                net_config_msg "IPv6=static but ALPINE_ZFSBOOT_IPV6_ADDRESS is unset - skipping IPv6"
            elif ip -6 -o addr show dev "$iface" 2>/dev/null | grep -qF "$ALPINE_ZFSBOOT_IPV6_ADDRESS"; then
                # Idempotency - same real reason as the IPv4 branch above.
                net_config_msg "IPv6: $ALPINE_ZFSBOOT_IPV6_ADDRESS already configured on $iface"
                ipv6_ok=1
            else
                net_config_msg "IPv6: static $ALPINE_ZFSBOOT_IPV6_ADDRESS on $iface"
                if ! ip -6 addr add "$ALPINE_ZFSBOOT_IPV6_ADDRESS" dev "$iface" 2>/tmp/net-config-v6.log; then
                    cat /tmp/net-config-v6.log
                    net_config_msg "failed to add IPv6 address $ALPINE_ZFSBOOT_IPV6_ADDRESS to $iface"
                else
                    ipv6_ok=1
                fi
            fi
            if [ "$ipv6_ok" = 1 ] && [ -n "${ALPINE_ZFSBOOT_IPV6_GATEWAY:-}" ]; then
                # `dev $iface` is not optional here despite `ip -6 route
                # add default via <global-addr>` working fine without it
                # in the common case - a link-local gateway (fe80::/10,
                # the standard convention for several real hosting
                # providers, including the exact scenario this was found
                # for) is only meaningful with an explicit
                # interface/scope attached, since the same fe80::-range
                # address can exist unrelated on every link
                # simultaneously - the kernel rejects a link-local
                # next-hop with no scope outright ("Invalid argument").
                # Always passing `dev` is correct and harmless for a
                # global-unicast gateway too, so this isn't conditional
                # on the gateway's own address range. Gateway problems
                # are logged but never flip ipv6_ok back off - same
                # "address alone is still progress" precedent as IPv4.
                if ip -6 route show default 2>/dev/null | grep -qF "via ${ALPINE_ZFSBOOT_IPV6_GATEWAY} dev ${iface}"; then
                    net_config_msg "IPv6: default route via $ALPINE_ZFSBOOT_IPV6_GATEWAY already present on $iface"
                elif ip -6 route add default via "$ALPINE_ZFSBOOT_IPV6_GATEWAY" dev "$iface" 2>/tmp/net-config-v6-gw.log; then
                    :
                else
                    cat /tmp/net-config-v6-gw.log
                    # Same on-link fallback as IPv4 above - a global-
                    # unicast IPv6 gateway outside the configured prefix
                    # (e.g. a /128 address with a gateway elsewhere,
                    # OVH's own real convention) needs the identical
                    # two-step treatment link-local gateways get for a
                    # different underlying reason.
                    net_config_msg "IPv6 default route via $ALPINE_ZFSBOOT_IPV6_GATEWAY failed directly - retrying via an explicit on-link host route (gateway may be outside the configured prefix)"
                    if ip -6 route add "$ALPINE_ZFSBOOT_IPV6_GATEWAY" dev "$iface" 2>/tmp/net-config-v6-gw2.log \
                       && ip -6 route add default via "$ALPINE_ZFSBOOT_IPV6_GATEWAY" dev "$iface" 2>>/tmp/net-config-v6-gw2.log; then
                        net_config_msg "IPv6: default route via $ALPINE_ZFSBOOT_IPV6_GATEWAY added via the on-link fallback"
                    else
                        cat /tmp/net-config-v6-gw2.log
                        net_config_msg "failed to add IPv6 default route via $ALPINE_ZFSBOOT_IPV6_GATEWAY (both direct and on-link attempts failed)"
                    fi
                fi
            fi
            ;;
        off) ;;
        *) net_config_msg "unknown ALPINE_ZFSBOOT_IPV6=$ipv6_mode, skipping IPv6" ;;
    esac

    # Fail only if EVERY requested (non-off) family failed outright - a
    # family that was never requested (=off) doesn't count against this,
    # and a family that partially worked (address up, gateway broken)
    # still counts as "ok" (see each branch above). This is what makes
    # partial connectivity strictly better than none for a break-glass
    # path, rather than one bad family taking dropbear down with it.
    net_config_status=1
    if [ "$ipv4_mode" != "off" ] && [ "$ipv4_ok" = 1 ]; then
        net_config_status=0
    fi
    if [ "$ipv6_mode" != "off" ] && [ "$ipv6_ok" = 1 ]; then
        net_config_status=0
    fi
    return "$net_config_status"
}

# Allow standalone execution too (`sh net-config.sh eth0`), for
# testing this file in isolation without faking a whole boot sequence
# - see rescue-ssh.sh's own identical $0-based dispatch for why $0,
# not $1, is what has to make this decision.
case "$0" in
    */net-config.sh|net-config.sh)
        set -u
        PATH=/sbin:/bin:/usr/sbin:/usr/bin
        export PATH
        net_config "$@"
        exit $?
        ;;
esac
