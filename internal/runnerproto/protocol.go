//go:generate go run ../../cmd/contractgen -root ../..

package runnerproto

func Frame(kind string) Message { return Message{Version: Version, Type: kind} }
