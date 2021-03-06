package main

import (
	"encoding/binary"
	"log"
	"runtime"
	"time"
	"unsafe"

	"github.com/songgao/water"
)

type scstInstance struct {
	logger               *log.Logger
	antiGCBufferStorage  map[int][]byte
	buffersMade          int
	currentpbuf          []byte
	globalOutputBuf      *[9728 + 8192]byte
	globalOutputBufAlign int
	ticker               <-chan time.Time
	tuntap               *water.Interface
}

func (instance *scstInstance) processExecCmd(in *raw_scst_user_get_cmd_scsi_cmd_exec) *raw_scst_user_reply_cmd_exec_reply_sense {
	/*
		(gdb) print *cmd
		$4 = {
		  sess_h = 18446614906040681408,
		  cdb = '\000' <repeats 15 times>,
		  cdb_len = 6,
		  lba = 0,
		  data_len = 0,
		  bufflen = 0,
		  alloc_len = 0,
		  pbuf = 0,
		  queue_type = 3 '\003',
		  data_direction = 4 '\004',
		  partial = 0 '\000',
		  timeout = 10,
		  p_out_buf = 0,
		  out_bufflen = 0,
		  sn = 0,
		  parent_cmd_h = 0,
		  parent_cmd_data_len = 0,
		  partial_offset = 0
		}
		(gdb) print *reply
		$5 = {
			resp_data_len = 0,
			pbuf = 0,
			reply_type = 1 '\001',
			status = 0 '\000',
			{
				{
					sense_len = 0 '\000',
					psense_buffer = 0
				},
				{
					ws_descriptors_len = 0,
					ws_descriptors = 0
				}
			}
		}

	*/
	if *debugLogs {
		instance.logger.Printf("cmd_h:%x, subcode:%x lba:%d, data_len:%d, bufflen:%d, alloc_len:%d, pbuf:%#v, queue_type:%x, data_direction:%x, partial:%x,\n timeout:%d, p_out_buf:%#v, out_bufflen:%d\ncdb (%d):%#v",
			in.cmd_h, in.subcode, in.lba, in.data_len, in.bufflen, in.alloc_len, in.pbuf, in.queue_type, in.data_direction, in.partial, in.timeout, in.p_out_buf, in.out_bufflen, in.cdb_len, in.cdb)
	}
	ATAopCode := in.cdb[0]

	reply := raw_scst_user_reply_cmd_exec_reply_sense{
		cmd_h:         in.cmd_h,
		subcode:       in.subcode,
		reply_type:    SCST_EXEC_REPLY_COMPLETED,
		resp_data_len: 0,
		pbuf:          nil,
		status:        SAM_STAT_GOOD,
	}

	if (in.alloc_len != 0 && in.pbuf == nil) || len(instance.antiGCBufferStorage) == 0 {
		// ooh, we need to alloc a buffer?
		// For this we need to make a page aligned buffer.
		// and set it to the reply of the responce.
		if *debugLogs {
			instance.logger.Printf("The module wishes for more memory sir.")
		}
		instance.buffersMade++

		aaa := make([]byte, in.alloc_len+8196)
		if in.alloc_len == 0 {
			aaa = make([]byte, 8196*2)
		}

		finalOutputOffset := alignTheBuffer(uintptr(unsafe.Pointer(&aaa[0])))

		reply.pbuf = &aaa[finalOutputOffset]
		instance.antiGCBufferStorage[instance.buffersMade] = aaa
		instance.currentpbuf = aaa[finalOutputOffset:]
	}

	if in.data_direction == 2 { // READ
		reply.resp_data_len = in.bufflen
		if *debugLogs {
			instance.logger.Printf("data_direction READ")
		}
	} else if in.data_direction == 4 { // None
		reply.resp_data_len = 0
		if *debugLogs {
			instance.logger.Printf("data_direction NONE")
		}
	} else {
		if *debugLogs {
			instance.logger.Printf("data_direction WRITE")
		}
	}

	if *debugLogs {
		instance.logger.Printf("------> Opcode %x", ATAopCode)
	}

	switch ATAopCode {
	case ATA_TEST_UNIT_READY:
		if *debugLogs {
			instance.logger.Printf("ATA_TEST_UNIT_READY")
		}
		// Do nothing???
	case ATA_INQUIRY:
		if *debugLogs {
			instance.logger.Printf("ATA_INQUIRY")
		}
		handleATAinquiry(in, &reply)
	case ATA_READ_CAPACITY:
		if *debugLogs {
			instance.logger.Printf("ATA_READ_CAPACITY")
		}
		handleATAreadCapacity(in, &reply)
	case ATA_MODE_SENSE:
		if *debugLogs {
			instance.logger.Printf("ATA_SENSE")
		}
		handleATAsense(in, &reply)
	case ATA_WRITE_16:
		if *debugLogs {
			instance.logger.Printf("ATA_WRITE")
		}
		instance.handleATAwrite(in, &reply)
	case ATA_READ_16:
		if *debugLogs {
			instance.logger.Printf("ATA_READ")
		}
		instance.handleATAread(in, &reply)
	default:
		if *debugLogs {
			instance.logger.Printf("Unsupported ATA opcode: %d / %x", ATAopCode, ATAopCode)
		}

		reply.reply_type = SAM_STAT_CHECK_CONDITION
		sense := [252]byte{}

		sense[0] = 0x70  /* Error Code			*/
		sense[2] = 0x05  /* Sense Key			*/ //  ILLEGAL_REQUEST
		sense[7] = 0x0a  /* Additional Sense Length	*/
		sense[12] = 0x24 /* ASC				*/
		sense[13] = 0x00 /* ASCQ				*/
		reply.sense_len = 18
		reply.psense_buffer = &sense[0]

		if *debugLogs {
			instance.logger.Printf("/* WARNING: Sending ILLEGAL_REQUEST SENSE */")
		}
	}

	runtime.KeepAlive(reply)
	return &reply
}

func handleATAsense(in *raw_scst_user_get_cmd_scsi_cmd_exec, reply *raw_scst_user_reply_cmd_exec_reply_sense) {
	// Fucked up and non functional SENSE
	var finalOutput [8192]byte
	output := make([]byte, in.bufflen)

	resp_len := 256

	offset := 0

	devtype := DEVICE_TYPE_SCANNER /* type dev */
	dbd := in.cdb[1] & 0x08
	pcode := in.cdb[2] & 0x3f
	msense_6 := (0x1a == in.cdb[0])
	dev_spec := 0x80

	blocksize := 512

	if pcode == 0x03 {
		// Unhandleable
		log.Fatalf("boom %v pcode == 0x03", in)
	}

	if msense_6 {
		output[1] = byte(devtype)
		output[2] = byte(dev_spec)
		offset = 4
	} else {
		output[2] = byte(devtype)
		output[3] = byte(dev_spec)
		offset = 8
	}

	dbd2 := dbd > 0x01
	if !dbd2 {
		/* Create block descriptor */
		output[offset-1] = 0x08 /* block descriptor length */
		output[offset+0] = 0xFF
		output[offset+1] = 0xFF
		output[offset+2] = 0xFF
		output[offset+3] = 0xFF
		output[offset+4] = 0                             /* density code */
		output[offset+5] = byte(blocksize>>(8*2)) & 0xFF /* blklen */
		output[offset+6] = byte(blocksize>>(8*1)) & 0xFF
		output[offset+7] = byte(blocksize>>(8*0)) & 0xFF

		offset += 8 /* increment offset */
	}

	log.Printf("pcode = %d", pcode)

	pcodelen := 0
	bpSlice := output[offset:]

	switch pcode {
	case 0x01:
		/* Read-Write Error Recovery page for mode_sense */
		a := [...]byte{0x1, 0xa, 0xc0, 11, 240, 0, 0, 0, 5, 0, 0xff, 0xff}
		for i, v := range a {
			bpSlice[i] = v
		}
		pcodelen = len(a)
		break
	case 0x02:
		/* Disconnect-Reconnect page, all devices */
		a := [...]byte{0x3, 0x16, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0,
			0, 0, 0, 0, 0x40, 0, 0, 0}
		for i, v := range a {
			bpSlice[i] = v
		}
		pcodelen = len(a)

		break
	case 0x03:
		/* Format device page, direct access */
		break
	case 0x04:
		/* Rigid disk geometry */
		a := [...]byte{0x04, 0x16, 0, 0, 0, 255, 0, 0,
			0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			0x3a, 0x98 /* 15K RPM */, 0, 0}
		for i, v := range a {
			bpSlice[i] = v
		}
		pcodelen = len(a)

		break
	case 0x08:
		/* Caching page, direct access */
		break
	case 0x0a:
		/* Control Mode page, all devices */
		break
	case 0x1c:
		/* Informational Exceptions Mode page, all devices */
		break
	case 0x3f:
		/* Read all Mode pages */
		break
	}

	offset += pcodelen

	if msense_6 {
		output[0] = byte(offset - 1)
	} else {
		output[0] = byte((offset-2)>>8) & 0xff
		output[1] = byte(offset-2) & 0xff
	}

	// ---
	log.Printf("debug: resp_len = %d", resp_len)

	reply.pbuf = &finalOutput[0]
	finalOutputOffset := alignTheBuffer(uintptr(unsafe.Pointer(in.pbuf)))

	copy(finalOutput[finalOutputOffset:], output[:offset])

	reply.pbuf = &finalOutput[finalOutputOffset]

	reply.resp_data_len = int32(offset)
	runtime.KeepAlive(finalOutput)
}

func alignTheBuffer(ptr uintptr) int {
	// AAAAAAAAAAAAAAAAAAA, NOoooOOoOoOOoO you can't possibly do this ben?

	// Well I need to because otherwise I will get page alignment errors from the module:
	// ***ERROR***: Supplied pbuf c00003bcc8 isn't page aligned

	// So we are just going to take a random roll of the PAGE_SIZE dice

	// [00:13:05] ben@metropolis:~$ getconf PAGE_SIZE
	// 4096

	// Then overAllocate by a lot, and provide a offset to use to the underlying application

	// lol.

	offset := int(ptr % 4096)
	offset2 := int(offset-4096) * -1
	offset3 := uintptr(offset2)

	return int(offset3)
}

func handleATAreadCapacity(in *raw_scst_user_get_cmd_scsi_cmd_exec, reply *raw_scst_user_reply_cmd_exec_reply_sense) {
	var finalOutput [8192]byte
	output := make([]byte, 8)
	blocksize := 512

	reply.status = SCST_EXEC_REPLY_COMPLETED

	output[0] = 0xFF
	output[1] = 0xFF
	output[2] = 0xFF
	output[3] = 0xFF
	output[4] = byte(blocksize>>(8*3)) & 0xFF
	output[5] = byte(blocksize>>(8*2)) & 0xFF
	output[6] = byte(blocksize>>(8*1)) & 0xFF
	output[7] = byte(blocksize>>(8*0)) & 0xFF

	resp_len := 8
	log.Printf("debug: resp_len = %d", resp_len)

	reply.pbuf = &finalOutput[0]
	finalOutputOffset := alignTheBuffer(uintptr(unsafe.Pointer(&finalOutput)))

	copy(finalOutput[finalOutputOffset:], output[:])

	reply.pbuf = &finalOutput[finalOutputOffset]

	reply.resp_data_len = int32(resp_len)

	runtime.KeepAlive(finalOutput)
}

func (instance *scstInstance) handleATAwrite(in *raw_scst_user_get_cmd_scsi_cmd_exec, reply *raw_scst_user_reply_cmd_exec_reply_sense) {
	if *debugLogs {
		instance.logger.Printf("WRITE !!!!!!!!!!!!!!!")
	}
	if *debugLogs {
		instance.logger.Printf("Incoming PACKET######################################")
	}
	// ignore the "misuse of unsafe.pointer", the linter is wrong.
	realPkt := make([]byte, 32768)
	InboundData := (*[32768]byte)(unsafe.Pointer(in.pbuf))

	copy(realPkt, InboundData[:])
	PktLen := binary.BigEndian.Uint16(realPkt[:2])

	instance.tuntap.Write(realPkt[2 : 2+PktLen])

	reply.status = SCST_EXEC_REPLY_COMPLETED
}

func (instance *scstInstance) babysitTunTapReads() {
	outboundPackets = make(chan []byte, 32)
	buf := make([]byte, 512*19)
	for {
		n, err := instance.tuntap.Read(buf)
		if err == nil {
			outboundPackets <- buf[:n]
		}
	}
}

func (instance *scstInstance) handleATAread(in *raw_scst_user_get_cmd_scsi_cmd_exec, reply *raw_scst_user_reply_cmd_exec_reply_sense) {
	if *debugLogs {
		instance.logger.Printf("READ !!!!!!!!!!!!!!!!")
	}
	var realpkt []byte
	var hasPacket bool
	var waitedSeconds int
	breakNow := false

	for {
		if breakNow {
			break
		}
		select {
		case pkt := <-outboundPackets:
			realpkt = make([]byte, len(pkt))
			copy(realpkt, pkt)
			hasPacket = true
			if *debugLogs {
				instance.logger.Printf("SENDING OUTBOUND PACKET(!)!(!)(!)!()!(!)!()!(!)(!)!s")
			}
			breakNow = true
			break
		case <-instance.ticker:
			waitedSeconds++
			if *debugLogs {
				instance.logger.Printf("waited %d seconds for a packet to arrive", waitedSeconds)
			}
			if waitedSeconds > 4 {
				breakNow = true
			}
		}
	}

	// outboundData[0] = 0xBE
	reply.resp_data_len = 512 * 19
	reply.sense_len = 0

	if instance.globalOutputBufAlign == -1 {
		instance.globalOutputBuf = new([9728 + 8192]byte)
		aa := alignTheBuffer(uintptr(unsafe.Pointer(instance.globalOutputBuf)))
		instance.globalOutputBufAlign = aa
	}
	if hasPacket {
		var PLenbytes [2]byte
		binary.BigEndian.PutUint16(PLenbytes[:], uint16(len(realpkt)))
		instance.globalOutputBuf[instance.globalOutputBufAlign] = PLenbytes[0]
		instance.globalOutputBuf[instance.globalOutputBufAlign+1] = PLenbytes[1]
		for i := 0; i < len(realpkt); i++ {
			instance.globalOutputBuf[instance.globalOutputBufAlign+2+i] = realpkt[i]
		}
	} else {
		for i := 0; i < 1536; i++ {
			instance.globalOutputBuf[instance.globalOutputBufAlign+i] = 0x00
		}
	}

	reply.pbuf = &instance.globalOutputBuf[instance.globalOutputBufAlign]
}

func handleATAinquiry(in *raw_scst_user_get_cmd_scsi_cmd_exec, reply *raw_scst_user_reply_cmd_exec_reply_sense) {
	var finalOutput [8192]byte

	output := make([]byte, in.bufflen)
	resp_len := 0

	output[0] = DEVICE_TYPE_SCANNER // sure, i'm a uhhh scanner
	if (in.cdb[1] & 0x01) > 1 {
		log.Printf("Inquire 1")

		if 0 == in.cdb[2] { /* supported vital product data pages */
			// Aka, "Hi frien, what do you support"
			log.Printf("Inquire 2")

			output[3] = 5
			output[4] = 0x0  /* this page */
			output[5] = 0x80 /* unit serial number */
			// output[6] = 0x83 /* device identification */
			output[7] = 0xB0 /* block limits */
			output[8] = 0xB1 /* block device characteristics */
			resp_len = int(uint8(output[3]) + 6)

		} else if 0x80 == in.cdb[2] { /* unit serial number */
			log.Printf("Inquire 3")

			/*
				unsigned int usn_len = strlen(dev->usn);
				buf[1] = 0x80;
				buf[3] = usn_len;
				assert(4 + usn_len <= sizeof(buf));
				memcpy(&buf[4], dev->usn, usn_len);
				resp_len = buf[3] + 4;
			*/
			output[1] = 0x80
			output[3] = 0x02
			output[4] = 0x33
			output[5] = 0x34
			resp_len = int(output[3]) + 4

		} else if 0x83 == in.cdb[2] { /* device identification */
			log.Printf("Inquire 4")

		} else if 0xB0 == in.cdb[2] { /* Block Limits */
			log.Printf("Inquire 5")

			max_transfer := 0
			// /* Block Limits */
			// int max_transfer;
			output[1] = 0xB0
			output[3] = 0x3C
			output[5] = 0xFF /* No MAXIMUM COMPARE AND WRITE LENGTH limit */
			// /* Optimal transfer granuality is PAGE_SIZE */
			max_transfer = 4096 / 1000000
			output[6] = byte(max_transfer>>8) & 0xff
			output[7] = byte(max_transfer) & 0xff
			// /*
			//  * Max transfer len is min of sg limit and 8M, but we
			//  * don't have access to them here, so let's use 1M.
			//  */
			max_transfer = 1 * 1024 * 1024
			output[8] = byte(max_transfer>>24) & 0xff
			output[9] = byte(max_transfer>>16) & 0xff
			output[10] = byte(max_transfer>>8) & 0xff
			output[11] = byte(max_transfer) & 0xff
			// /*
			//  * Let's have optimal transfer len 512KB. Better to not
			//  * set it at all, because we don't have such limit,
			//  * but some initiators may not understand that (?).
			//  * From other side, too big transfers  are not optimal,
			//  * because SGV cache supports only <4M buffers.
			//  */
			max_transfer = (512 * 1024)
			output[12] = byte(max_transfer>>24) & 0xff
			output[13] = byte(max_transfer>>16) & 0xff
			output[14] = byte(max_transfer>>8) & 0xff
			output[15] = byte(max_transfer) & 0xff
			resp_len = int(output[3]) + 4

		} else if 0xB1 == in.cdb[2] { /* Block Device Characteristics */
			log.Printf("Inquire 6")
			output[1] = 0xB1
			output[3] = 0x3C

			/* 15K RPM */
			// r = 0x3A98;
			r := 0x0045
			// r = 1
			output[4] = byte(r>>8) & 0xff
			output[5] = byte(r & 0xff)
			resp_len = int(output[3]) + 4

		} else {
			log.Printf("Inquire 7")

			// unsupported
		}

	} else {
		// Really basic stuff:
		log.Printf("Inquire 8")

		if in.cdb[2] != 0 {
			// TRACE_DBG("INQUIRY: Unsupported page %x", cmd->cdb[2]);
			// PRINT_INFO("INQUIRY: Unsupported page %x", cmd->cdb[2]);
			// set_cmd_error(vcmd,
			//     SCST_LOAD_SENSE(scst_sense_invalid_field_in_cdb));
			// goto out;
			// return reply
			// reply.sense_len
			log.Printf("ATA INQUIRE: Unsupported INQ PAGE")

			reply.reply_type = SAM_STAT_CHECK_CONDITION
			// reply.sense_len

			sense := [252]byte{}

			sense[0] = 0x70  /* Error Code			*/
			sense[2] = 0x05  /* Sense Key			*/ //  ILLEGAL_REQUEST
			sense[7] = 0x0a  /* Additional Sense Length	*/
			sense[12] = 0x24 /* ASC				*/
			sense[13] = 0x00 /* ASCQ				*/
			reply.sense_len = 18
			reply.psense_buffer = &sense[0]

			log.Printf("/* WARNING: Sending ILLEGAL_REQUEST SENSE */")

		}

		output[2] = 6    /* Device complies to SPC-4 */
		output[3] = 0x12 /* HiSup + data in format specified in SPC */
		output[4] = 31   /* n - 4 = 35 - 4 = 31 for full 36 byte data */
		output[6] = 1    /* MultiP 1 */
		output[7] = 2    /* CMDQUE 1, BQue 0 => commands queuing supported */

		copy(output[8:], []byte("BENJOJO "))
		/* 8 byte ASCII Vendor Identification of the target - left aligned */
		// memcpy(&buf[8], VENDOR, 8);

		/* 16 byte ASCII Product Identification of the target - left aligned */
		copy(output[16:], []byte("                "))
		copy(output[16:], []byte("Network Card lol"))
		// memset(&buf[16], ' ', 16);
		// len = min(strlen(dev->name), (size_t)16);
		// memcpy(&buf[16], dev->name, len);

		/* 4 byte ASCII Product Revision Level of the target - left aligned */
		// memcpy(&buf[32], FIO_REV, 4);
		copy(output[32:], []byte("350 "))

		resp_len = int(output[4]) + 5

		// */
	}

	log.Printf("debug: resp_len = %d", resp_len)

	finalOutputOffset := alignTheBuffer(uintptr(unsafe.Pointer(&finalOutput)))

	copy(finalOutput[finalOutputOffset:], output[:])

	in.pbuf = &finalOutput[finalOutputOffset]
	reply.pbuf = &finalOutput[finalOutputOffset]

	reply.resp_data_len = int32(resp_len)
	runtime.KeepAlive(finalOutput)
}

const (
	ATA_TEST_UNIT_READY       = 0x00
	ATA_REZERO_UNIT           = 0x01
	ATA_REQUEST_SENSE         = 0x03
	ATA_FORMAT_UNIT           = 0x04
	ATA_READ_BLOCK_LIMITS     = 0x05
	ATA_REASSIGN_BLOCKS       = 0x07
	ATA_READ_6                = 0x08
	ATA_WRITE_6               = 0x0a
	ATA_SEEK_6                = 0x0b
	ATA_READ_REVERSE          = 0x0f
	ATA_WRITE_FILEMARKS       = 0x10
	ATA_SPACE                 = 0x11
	ATA_INQUIRY               = 0x12
	ATA_RECOVER_BUFFERED_DATA = 0x14
	ATA_MODE_SELECT           = 0x15
	ATA_RESERVE               = 0x16
	ATA_RELEASE               = 0x17
	ATA_COPY                  = 0x18
	ATA_ERASE                 = 0x19
	ATA_MODE_SENSE            = 0x1a
	ATA_START_STOP            = 0x1b
	ATA_RECEIVE_DIAGNOSTIC    = 0x1c
	ATA_SEND_DIAGNOSTIC       = 0x1d
	ATA_ALLOW_MEDIUM_REMOVAL  = 0x1e

	ATA_SET_WINDOW             = 0x24
	ATA_READ_CAPACITY          = 0x25
	ATA_READ_10                = 0x28
	ATA_WRITE_10               = 0x2a
	ATA_SEEK_10                = 0x2b
	ATA_WRITE_VERIFY           = 0x2e
	ATA_VERIFY                 = 0x2f
	ATA_SEARCH_HIGH            = 0x30
	ATA_SEARCH_EQUAL           = 0x31
	ATA_SEARCH_LOW             = 0x32
	ATA_SET_LIMITS             = 0x33
	ATA_PRE_FETCH              = 0x34
	ATA_READ_POSITION          = 0x34
	ATA_SYNCHRONIZE_CACHE      = 0x35
	ATA_LOCK_UNLOCK_CACHE      = 0x36
	ATA_READ_DEFECT_DATA       = 0x37
	ATA_MEDIUM_SCAN            = 0x38
	ATA_COMPARE                = 0x39
	ATA_COPY_VERIFY            = 0x3a
	ATA_WRITE_BUFFER           = 0x3b
	ATA_READ_BUFFER            = 0x3c
	ATA_UPDATE_BLOCK           = 0x3d
	ATA_READ_LONG              = 0x3e
	ATA_WRITE_LONG             = 0x3f
	ATA_CHANGE_DEFINITION      = 0x40
	ATA_WRITE_SAME             = 0x41
	ATA_READ_TOC               = 0x43
	ATA_LOG_SELECT             = 0x4c
	ATA_LOG_SENSE              = 0x4d
	ATA_MODE_SELECT_10         = 0x55
	ATA_RESERVE_10             = 0x56
	ATA_RELEASE_10             = 0x57
	ATA_MODE_SENSE_10          = 0x5a
	ATA_PERSISTENT_RESERVE_IN  = 0x5e
	ATA_PERSISTENT_RESERVE_OUT = 0x5f
	ATA_READ_16                = 0x88
	ATA_WRITE_16               = 0x8a
	ATA_MOVE_MEDIUM            = 0xa5
	ATA_READ_12                = 0xa8
	ATA_WRITE_12               = 0xaa
	ATA_WRITE_VERIFY_12        = 0xae
	ATA_SEARCH_HIGH_12         = 0xb0
	ATA_SEARCH_EQUAL_12        = 0xb1
	ATA_SEARCH_LOW_12          = 0xb2
	ATA_READ_ELEMENT_STATUS    = 0xb8
	ATA_SEND_VOLUME_TAG        = 0xb6
	ATA_WRITE_LONG_2           = 0xea

	SAM_STAT_GOOD                       = 0x00
	SAM_STAT_CHECK_CONDITION            = 0x02
	SAM_STAT_CONDITION_MET              = 0x04
	SAM_STAT_BUSY                       = 0x08
	SAM_STAT_INTERMEDIATE               = 0x10
	SAM_STAT_INTERMEDIATE_CONDITION_MET = 0x14
	SAM_STAT_RESERVATION_CONFLICT       = 0x18
	SAM_STAT_COMMAND_TERMINATED         = 0x22 /* obsolete in SAM-3 */
	SAM_STAT_TASK_SET_FULL              = 0x28
	SAM_STAT_ACA_ACTIVE                 = 0x30
	SAM_STAT_TASK_ABORTED               = 0x40
)
