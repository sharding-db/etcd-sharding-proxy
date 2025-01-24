# case 1

reproduce: steps:

etcdctl lease grant 100
etcdctl put a 1 --lease=00009497fc455902
etcdctl put z 2 --lease=00009497fc455902
etcdctl put l 3 --lease=00009497fc455902

wait for a few seconds

watch was canceled (rpc error: code = Unknown desc = closing transport due to: connection error: desc = "error reading from server: EOF", received prior goaway: code: ENHANCE_YOUR_CALM, debug data: too_many_pings)
Error: watch is canceled by the server