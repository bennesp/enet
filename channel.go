package enet

const channel_packet_count = 256

type enet_channel_item struct {
	header   EnetPacketHeader
	fragment EnetPacketFragment // used if header.cmd == enet_packet_fragment
	payload  []byte             // not include packet-header
	retries  int                // sent times for outgoing packet
	acked    int                // acked times
	retrans  *enet_timer_item   // retrans timer
}

// outgoing: ->end ..untransfered.. next ..transfered.. begin ->
// incoming: <-begin ..acked.. next ..unacked.. end<-
type enet_channel struct {
	_next_sn       uint32 // next reliable packet number for sent
	_next_usn      uint32 // next unsequenced packet number for sent
	outgoing       [channel_packet_count]*enet_channel_item
	incoming       [channel_packet_count]*enet_channel_item
	outgoing_begin uint32 // the first one is not acked yet
	incoming_begin uint32 // the first one has be received
	outgoing_end   uint32 // the last one is not acked yet
	incoming_end   uint32 // the last one has been received
	outgoing_used  uint32 // in trans packets not acked
	incoming_used  uint32 // rcvd packet count in incoming window
	outgoing_next  uint32 // the next one is being to send first time
	intrans_bytes  uint32
}

func (ch *enet_channel) outgoing_pend(item *enet_channel_item) {
	if ch._next_sn == 0 {
		ch._next_sn = 1
		ch.outgoing_begin = 1
		ch.incoming_begin = 1
		ch.outgoing_end = 1
		ch.incoming_end = 1
		ch.outgoing_used = 1
		ch.incoming_used = 1
		ch.outgoing_next = 1
	}
	item.header.SN = ch._next_sn
	ch._next_sn++
	debugf("channel outgoing %v, typ: %v\n", item.header.SN, item.header.Type)
	idx := item.header.SN % channel_packet_count
	v := ch.outgoing[idx]
	assert(v == nil && item.header.SN == ch.outgoing_end)
	ch.outgoing[idx] = item
	if ch.outgoing_end <= item.header.SN {
		ch.outgoing_end = item.header.SN + 1
	}
	ch.outgoing_used++
}

// what if outgoing_wrap
func (ch *enet_channel) outgoing_ack(sn uint32) {
	debugf("outgoing ack %v\n", sn)
	if sn < ch.outgoing_begin || sn >= ch.outgoing_end { // already acked or error
		debugf("channel-ack abandoned %v\n", sn)
		return
	}
	idx := sn % channel_packet_count
	v := ch.outgoing[idx]
	assert(v != nil && v.header.SN == sn)
	ch.intrans_bytes -= v.header.Size
	v.acked++
}

func (ch *enet_channel) outgoing_slide() (item *enet_channel_item) {
	assert(ch.outgoing_begin <= ch.outgoing_end)
	if ch.outgoing_begin >= ch.outgoing_end {
		return
	}
	idx := ch.outgoing_begin % channel_packet_count
	v := ch.outgoing[idx]
	assert(v != nil)
	if v.retries == 0 {
		return
	}
	if v.header.Type != enet_packet_type_ack && v.acked == 0 {
		return
	}
	debugf("outgoing slide %v, sn:%v, rty:%v, ack:%v\n", v.header.Type, v.header.SN, v.retries, v.acked)
	item = v
	ch.outgoing_begin++
	return
}

// the first time send out packet
func (ch *enet_channel) outgoing_do_trans() (item *enet_channel_item) {
	assert(ch.outgoing_next <= ch.outgoing_end)
	if ch.outgoing_next >= ch.outgoing_end {
		return
	}
	idx := ch.outgoing_next % channel_packet_count
	item = ch.outgoing[idx]
	assert(item != nil)
	assert((item.acked == 0 && item.header.Type != enet_packet_type_ack) || item.header.Type == enet_packet_type_ack)
	item.retries++
	ch.outgoing_next++
	ch.intrans_bytes += item.header.Size
	return
}

// may be retransed packet
func (ch *enet_channel) incoming_trans(item *enet_channel_item) {
	if item.header.SN < ch.incoming_begin {
		return
	}
	idx := item.header.SN % channel_packet_count
	v := ch.incoming[idx]
	// duplicated packet
	if v != nil {
		v.retries++
		return
	}
	assert(v == nil || v.header.SN == item.header.SN)

	ch.incoming[idx] = item
	ch.incoming_used++
	if ch.incoming_end <= item.header.SN {
		ch.incoming_end = item.header.SN + 1
	}
}

// when do ack incoming packets
func (ch *enet_channel) incoming_ack(sn uint32) {
	if sn < ch.incoming_begin || sn >= ch.incoming_end { // reack packet not in wnd
		return
	}
	idx := sn % channel_packet_count
	v := ch.incoming[idx]
	assert(v != nil && v.header.SN == sn)
	v.acked++
}

// called after incoming-ack
func (ch *enet_channel) incoming_slide() (item *enet_channel_item) { // return value may be ignored
	if ch.incoming_begin >= ch.incoming_end {
		return
	}
	idx := ch.incoming_begin % channel_packet_count
	v := ch.incoming[idx]
	if v == nil || v.acked <= 0 { // not received yet
		return
	}
	assert(v.header.SN == ch.incoming_begin)

	// merge fragments
	if v.header.Type == enet_packet_type_fragment {
		all := true
		for i := uint32(1); i < v.fragment.Count; i++ {
			n := ch.incoming[idx+i]
			if n == nil || n.header.SN != v.header.SN+i || n.fragment.SN != v.header.SN {
				all = false
				break
			}
		}
		if !all {
			return
		}

		item = v
		ch.incoming_begin += v.fragment.Count
		ch.incoming_used -= v.fragment.Count
		for i := uint32(1); i < v.fragment.Count; i++ {
			item.payload = append(item.payload, ch.incoming[idx+1].payload...)
			ch.incoming[idx+i] = nil
		}
		ch.incoming[idx] = nil

		return
	}
	item = v
	ch.incoming_begin++
	ch.incoming_used--
	ch.incoming[idx] = nil
	return
}

func (ch *enet_channel) do_send(peer *enet_peer) {
	if ch.intrans_bytes > peer.wnd_size { // window is overflow
		return
	}
	for item := ch.outgoing_do_trans(); item != nil; item = ch.outgoing_do_trans() {
		peer.do_send(item.header, item.fragment, item.payload)
		item.retries++
	}
}
