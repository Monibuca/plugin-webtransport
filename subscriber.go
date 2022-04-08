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
	case HaveFLV:
		flvTag := v.GetFLV()
		if _, err := flvTag.WriteTo(wt); err != nil {
			wt.Stop()
		}
	default:
		wt.Subscriber.OnEvent(event)
	}
}
