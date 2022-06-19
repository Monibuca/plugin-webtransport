package webtransport

import (
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/codec"
)

type WebTransportSubscriber struct {
	Subscriber
}

func (wt *WebTransportSubscriber) OnEvent(event any) {
	switch v := event.(type) {
	case ISubscriber:
		wt.Write(codec.FLVHeader)
	case FLVFrame:
		if _, err := v.WriteTo(wt); err != nil {
			wt.Stop()
		}
	default:
		wt.Subscriber.OnEvent(event)
	}
}
