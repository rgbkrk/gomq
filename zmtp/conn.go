package zmtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Connection struct {
	rw                         io.ReadWriter
	securityMechanism          SecurityMechanism
	socket                     Socket
	isPrepared                 bool
	asServer, otherEndAsServer bool
}

type SocketType string

const (
	ClientSocketType SocketType = "CLIENT"
	ServerSocketType SocketType = "SERVER"
)

func NewConnection(rw io.ReadWriter) *Connection {
	return &Connection{rw: rw}
}

func (c *Connection) Prepare(mechanism SecurityMechanism, socketType SocketType, asServer bool, applicationMetadata map[string]string) (map[string]string, error) {
	if c.isPrepared {
		return nil, errors.New("Connection was already prepared")
	}

	c.isPrepared = true
	c.securityMechanism = mechanism

	var err error
	if c.socket, err = NewSocket(socketType); err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while creating socket: %v", err)
	}

	// Send/recv greeting
	if err := c.sendGreeting(asServer); err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while sending greeting: %v", err)
	}
	if err := c.recvGreeting(asServer); err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while receiving greeting: %v", err)
	}

	// Do security handshake
	if err := mechanism.Handshake(); err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while running the security handshake: %v", err)
	}

	// Send/recv metadata
	if err := c.sendMetadata(socketType, applicationMetadata); err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while sending metadata: %v", err)
	}

	otherEndApplicationMetaData, err := c.recvMetadata()
	if err != nil {
		return nil, fmt.Errorf("gomq/zmtp: Got error while receiving metadata: %v", err)
	}

	return otherEndApplicationMetaData, nil
}

func (c *Connection) sendGreeting(asServer bool) error {
	greeting := greeting{
		SignaturePrefix: signaturePrefix,
		SignatureSuffix: signatureSuffix,
		Version:         version,
	}
	toNullPaddedString(string(c.securityMechanism.Type()), greeting.Mechanism[:])

	if err := binary.Write(c.rw, byteOrder, &greeting); err != nil {
		return err
	}

	return nil
}

func (c *Connection) recvGreeting(asServer bool) error {
	var greeting greeting

	if err := binary.Read(c.rw, byteOrder, &greeting); err != nil {
		return fmt.Errorf("Error while reading: %v", err)
	}

	if greeting.SignaturePrefix != signaturePrefix {
		return fmt.Errorf("Signature prefix received does not correspond with expected signature. Received: %#v. Expected: %#v.", greeting.SignaturePrefix, signaturePrefix)
	}

	if greeting.SignatureSuffix != signatureSuffix {
		return fmt.Errorf("Signature prefix received does not correspond with expected signature. Received: %#v. Expected: %#v.", greeting.SignatureSuffix, signatureSuffix)
	}

	if greeting.Version != version {
		return fmt.Errorf("Version %v.%v received does match expected version %v.%v", int(greeting.Version[0]), int(greeting.Version[1]), int(majorVersion), int(minorVersion))
	}

	var otherMechanism = fromNullPaddedString(greeting.Mechanism[:])
	var thisMechanism = string(c.securityMechanism.Type())
	if thisMechanism != otherMechanism {
		return fmt.Errorf("Encryption mechanism on other side %q does not match this side's %q", otherMechanism, thisMechanism)
	}

	otherEndAsServer, err := fromByteBool(greeting.ServerFlag)
	if err != nil {
		return err
	}
	c.otherEndAsServer = otherEndAsServer

	return nil
}

func (c *Connection) sendMetadata(socketType SocketType, applicationMetadata map[string]string) error {
	buffer := new(bytes.Buffer)
	var usedKeys map[string]struct{}

	for k, v := range applicationMetadata {
		if len(k) == 0 {
			return errors.New("Cannot send empty application metadata key")
		}

		lowerCaseKey := strings.ToLower(k)
		if _, alreadyPresent := usedKeys[lowerCaseKey]; alreadyPresent {
			return fmt.Errorf("Key %q is specified multiple times with different casing", lowerCaseKey)
		}

		usedKeys[lowerCaseKey] = struct{}{}
		c.writeMetadata(buffer, "x-"+lowerCaseKey, v)
	}

	c.writeMetadata(buffer, "socket-type", string(socketType))

	return c.SendCommand("READY", buffer.Bytes())
}

func (c *Connection) writeMetadata(buffer *bytes.Buffer, name string, value string) {
	buffer.WriteByte(byte(len(name)))
	buffer.WriteString(name)
	binary.Write(buffer, byteOrder, uint32(len(value)))
	buffer.WriteString(value)
}

func (c *Connection) recvMetadata() (map[string]string, error) {
	isCommand, body, err := c.read()
	if err != nil {
		return nil, err
	}

	if !isCommand {
		return nil, errors.New("Got a message frame for metadata, expected a command frame")
	}

	command, err := c.parseCommand(body)
	if err != nil {
		return nil, err
	}

	if command.Name != "READY" {
		return nil, fmt.Errorf("Got a %v command for metadata instead of the expected READY command frame", command.Name)
	}

	metadata := make(map[string]string)
	applicationMetadata := make(map[string]string)
	i := 0
	for i < len(command.Body) {
		// Key length
		keyLength := int(command.Body[i])
		if i+keyLength >= len(command.Body) {
			return nil, fmt.Errorf("metadata key of length %v overflows body of length %v at position %v", keyLength, len(command.Body), i)
		}
		i++

		// Key
		key := strings.ToLower(string(command.Body[i : i+keyLength]))
		i += keyLength

		// Value length
		var rawValueLength uint32
		if err := binary.Read(bytes.NewBuffer(command.Body[i:i+4]), byteOrder, &rawValueLength); err != nil {
			return nil, err
		}

		if uint64(rawValueLength) > uint64(maxInt) {
			return nil, fmt.Errorf("Length of value %v overflows integer max length %v on this platform", rawValueLength, maxInt)
		}

		valueLength := int(rawValueLength)
		if i+valueLength >= len(command.Body) {
			return nil, fmt.Errorf("metadata value of length %v overflows body of length %v at position %v", valueLength, len(command.Body), i)
		}
		i += 4

		// Value
		value := string(command.Body[i : i+valueLength])
		i += valueLength

		if strings.HasPrefix(key, "x-") {
			applicationMetadata[key[2:]] = value
		} else {
			metadata[key] = value
		}
	}

	socketType := metadata["socket-type"]
	if !c.socket.IsSocketTypeCompatible(SocketType(socketType)) {
		return nil, fmt.Errorf("Socket type %v is not compatible with %v", c.socket.Type(), socketType)
	}

	return applicationMetadata, nil
}

func (c *Connection) SendCommand(commandName string, body []byte) error {
	if len(commandName) > 255 {
		return errors.New("Command names may not be longer than 255 characters")
	}

	// Make the buffer of the correct lenght and reset it
	buffer := new(bytes.Buffer)
	buffer.WriteByte(byte(len(commandName)))
	buffer.Write([]byte(commandName))
	buffer.Write(body)

	return c.send(true, buffer.Bytes())
}

func (c *Connection) SendFrame(body []byte) error {
	return c.send(false, body)
}

func (c *Connection) send(isCommand bool, body []byte) error {
	// Compute total body length
	length := len(body)

	var bitFlags byte

	// More flag: Unused, we don't support multiframe messages

	// Long flag
	isLong := length > 255
	if isLong {
		bitFlags ^= isLongBitFlag
	}

	// Command flag
	if isCommand {
		bitFlags ^= isCommandBitFlag
	}

	// Write out the message itself
	if _, err := c.rw.Write([]byte{bitFlags}); err != nil {
		return err
	}

	if isLong {
		if err := binary.Write(c.rw, byteOrder, int64(len(body))); err != nil {
			return err
		}
	} else {
		if err := binary.Write(c.rw, byteOrder, uint8(len(body))); err != nil {
			return err
		}
	}

	if _, err := c.rw.Write(c.securityMechanism.Encrypt(body)); err != nil {
		return err
	}

	return nil
}

// Recv starts listening to the ReadWriter and returns two channels: The first one is for messages, the second one is for commands
func (c *Connection) Recv() (<-chan []byte, <-chan *Command, <-chan error) {
	messageOut := make(chan []byte)
	commandOut := make(chan *Command)
	errorOut := make(chan error)

	go func() {
		defer close(messageOut)
		defer close(commandOut)
		defer close(errorOut)

		for {
			// Actually read out the body and send it over the channel now
			isCommand, body, err := c.read()
			if err != nil {
				errorOut <- err
				return
			}

			if !isCommand {
				// Data frame
				messageOut <- body
			} else {
				command, err := c.parseCommand(body)
				if err != nil {
					errorOut <- err
					return
				}

				// Check what type of command we got
				// Certain commands we deal with directly, the rest we send over to the application
				switch command.Name {
				case "PING":
					// When we get a ping, we want to send back a pong, we don't really care about the contents right now
					if err := c.SendCommand("PONG", nil); err != nil {
						errorOut <- err
						return
					}
				default:
					commandOut <- command
				}

			}
		}
	}()

	return messageOut, commandOut, errorOut
}

// read returns the isCommand flag, the body of the message, and optionally an error
func (c *Connection) read() (bool, []byte, error) {
	var header [2]byte
	var longLength [4]byte

	// Read out the header
	readLength := uint64(0)
	for readLength != 2 {
		l, err := c.rw.Read(header[readLength:])
		if err != nil {
			return false, nil, err
		}

		readLength += uint64(l)
	}

	bitFlags := header[0]

	// Read all the flags
	hasMore := bitFlags&hasMoreBitFlag == hasMoreBitFlag
	isLong := bitFlags&isLongBitFlag == isLongBitFlag
	isCommand := bitFlags&isCommandBitFlag == isCommandBitFlag

	// Error out in case get a more flag set to true
	if hasMore {
		return false, nil, errors.New("Received a packet with the MORE flag set to true, we don't support more")
	}

	// Determine the actual length of the body
	bodyLength := uint64(0)
	if isLong {
		// We read 2 bytes of the header already
		// In case of a long message, the length is bytes 2-8 of the header
		// We already have the first byte, so assign it, and then read the rest
		longLength[0] = header[1]

		readLength := 1
		for readLength != 8 {
			l, err := c.rw.Read(longLength[readLength:])
			if err != nil {
				return false, nil, err
			}

			readLength += l
		}

		if err := binary.Read(bytes.NewBuffer(longLength[:]), byteOrder, &bodyLength); err != nil {
			return false, nil, err
		}
	} else {
		// Short message length is just 1 byte, read it
		bodyLength = uint64(header[1])
	}

	if bodyLength > uint64(maxInt64) {
		return false, nil, fmt.Errorf("Body length %v overflows max int64 value %v", bodyLength, maxInt64)
	}

	buffer := new(bytes.Buffer)
	readLength = 0
	for readLength < bodyLength {
		l, err := buffer.ReadFrom(io.LimitReader(c.rw, int64(bodyLength)-int64(readLength)))
		if err != nil {
			return false, nil, err
		}

		readLength += uint64(l)
	}

	return isCommand, buffer.Bytes(), nil
}

func (c *Connection) parseCommand(body []byte) (*Command, error) {
	// Sanity check
	if len(body) == 0 {
		return nil, errors.New("Got empty command frame body")
	}

	// Read out the command length
	commandNameLength := int(body[0])
	if commandNameLength > len(body)-1 {
		return nil, fmt.Errorf("Got command name length %v, which is too long for a body of length %v", commandNameLength, len(body))
	}

	command := &Command{
		Name: string(body[1 : commandNameLength+1]),
		Body: body[1+commandNameLength:],
	}

	return command, nil
}
