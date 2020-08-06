#!/usr/bin/env bash
# shellcheck disable=SC2128
# shellcheck source=/dev/null

source "./allRelayerTest.sh"
#source "./relayerTest.sh"
source "./perf_test.sh"

function x2ethereum() {
    if [ "${2}" == "init" ]; then
        return
    elif [ "${2}" == "config" ]; then
        return
    elif [ "${2}" == "test" ]; then
        echo "========================== x2ethereum test =========================="
        set +e
        set -x
        AllRelayerMainTest 5
#        perf_test_main 10
        echo "========================== x2ethereum test end =========================="
    fi
}
