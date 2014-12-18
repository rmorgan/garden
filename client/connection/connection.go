package connection

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"code.google.com/p/gogoprotobuf/proto"
	"github.com/cloudfoundry-incubator/garden/api"
	protocol "github.com/cloudfoundry-incubator/garden/protocol"
	"github.com/cloudfoundry-incubator/garden/routes"
	"github.com/cloudfoundry-incubator/garden/transport"
	"github.com/tedsuo/rata"
)

var ErrDisconnected = errors.New("disconnected")
var ErrInvalidMessage = errors.New("invalid message payload")

type Connection interface {
	Ping() error

	Capacity() (api.Capacity, error)

	Create(spec api.ContainerSpec) (string, error)
	List(properties api.Properties) ([]string, error)
	Destroy(handle string) error

	Stop(handle string, kill bool) error

	Info(handle string) (api.ContainerInfo, error)

	StreamIn(handle string, dstPath string, reader io.Reader) error
	StreamOut(handle string, srcPath string) (io.ReadCloser, error)

	LimitBandwidth(handle string, limits api.BandwidthLimits) (api.BandwidthLimits, error)
	LimitCPU(handle string, limits api.CPULimits) (api.CPULimits, error)
	LimitDisk(handle string, limits api.DiskLimits) (api.DiskLimits, error)
	LimitMemory(handle string, limit api.MemoryLimits) (api.MemoryLimits, error)

	CurrentBandwidthLimits(handle string) (api.BandwidthLimits, error)
	CurrentCPULimits(handle string) (api.CPULimits, error)
	CurrentDiskLimits(handle string) (api.DiskLimits, error)
	CurrentMemoryLimits(handle string) (api.MemoryLimits, error)

	Run(handle string, spec api.ProcessSpec, io api.ProcessIO) (api.Process, error)
	Attach(handle string, processID uint32, io api.ProcessIO) (api.Process, error)
	Kill(handle string, processID uint32) error

	NetIn(handle string, hostPort, containerPort uint32) (uint32, uint32, error)
	NetOut(handle string, network string, port uint32) error

	GetProperty(handle string, name string) (string, error)
	SetProperty(handle string, name string, value string) error
	RemoveProperty(handle string, name string) error
}

type connection struct {
	req *rata.RequestGenerator

	dialer func(string, string) (net.Conn, error)

	httpClient        *http.Client
	noKeepaliveClient *http.Client
}

type GardenError struct {
	Message   string
	Data      string
	Backtrace []string
}

func (e *GardenError) Error() string {
	return e.Message
}

func New(network, address string) Connection {
	dialer := func(string, string) (net.Conn, error) {
		return net.DialTimeout(network, address, time.Second)
	}

	return &connection{
		req: rata.NewRequestGenerator("http://api", routes.Routes),

		dialer: dialer,

		httpClient: &http.Client{
			Transport: &http.Transport{
				Dial: dialer,
			},
		},
		noKeepaliveClient: &http.Client{
			Transport: &http.Transport{
				Dial:              dialer,
				DisableKeepAlives: true,
			},
		},
	}
}

func (c *connection) Ping() error {
	return c.do(routes.Ping, nil, &protocol.PingResponse{}, nil, nil)
}

func (c *connection) Capacity() (api.Capacity, error) {
	capacity := &protocol.CapacityResponse{}

	err := c.do(routes.Capacity, nil, capacity, nil, nil)
	if err != nil {
		return api.Capacity{}, err
	}

	return api.Capacity{
		MemoryInBytes: capacity.GetMemoryInBytes(),
		DiskInBytes:   capacity.GetDiskInBytes(),
		MaxContainers: capacity.GetMaxContainers(),
	}, nil
}

func (c *connection) Create(spec api.ContainerSpec) (string, error) {
	req := &protocol.CreateRequest{}

	if spec.Handle != "" {
		req.Handle = proto.String(spec.Handle)
	}

	if spec.RootFSPath != "" {
		req.Rootfs = proto.String(spec.RootFSPath)
	}

	if spec.GraceTime != 0 {
		req.GraceTime = proto.Uint32(uint32(spec.GraceTime.Seconds()))
	}

	if spec.Network != "" {
		req.Network = proto.String(spec.Network)
	}

	if spec.Env != nil {
		req.Env = convertEnvironmentVariables(spec.Env)
	}

	req.Privileged = proto.Bool(spec.Privileged)

	for _, bm := range spec.BindMounts {
		var mode protocol.CreateRequest_BindMount_Mode
		var origin protocol.CreateRequest_BindMount_Origin

		switch bm.Mode {
		case api.BindMountModeRO:
			mode = protocol.CreateRequest_BindMount_RO
		case api.BindMountModeRW:
			mode = protocol.CreateRequest_BindMount_RW
		}

		switch bm.Origin {
		case api.BindMountOriginHost:
			origin = protocol.CreateRequest_BindMount_Host
		case api.BindMountOriginContainer:
			origin = protocol.CreateRequest_BindMount_Container
		}

		req.BindMounts = append(req.BindMounts, &protocol.CreateRequest_BindMount{
			SrcPath: proto.String(bm.SrcPath),
			DstPath: proto.String(bm.DstPath),
			Mode:    &mode,
			Origin:  &origin,
		})
	}

	props := []*protocol.Property{}
	for key, val := range spec.Properties {
		props = append(props, &protocol.Property{
			Key:   proto.String(key),
			Value: proto.String(val),
		})
	}

	req.Properties = props

	res := &protocol.CreateResponse{}
	err := c.do(routes.Create, req, res, nil, nil)
	if err != nil {
		return "", err
	}

	return res.GetHandle(), nil
}

func (c *connection) Stop(handle string, kill bool) error {
	return c.do(
		routes.Stop,
		&protocol.StopRequest{
			Handle: proto.String(handle),
			Kill:   proto.Bool(kill),
		},
		&protocol.StopResponse{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) Destroy(handle string) error {
	return c.do(
		routes.Destroy,
		nil,
		&protocol.DestroyResponse{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) Run(handle string, spec api.ProcessSpec, processIO api.ProcessIO) (api.Process, error) {
	reqBody := new(bytes.Buffer)

	var dir *string
	if spec.Dir != "" {
		dir = proto.String(spec.Dir)
	}

	var tty *protocol.TTY
	if spec.TTY != nil {
		tty = &protocol.TTY{}

		if spec.TTY.WindowSize != nil {
			tty.WindowSize = &protocol.TTY_WindowSize{
				Columns: proto.Uint32(uint32(spec.TTY.WindowSize.Columns)),
				Rows:    proto.Uint32(uint32(spec.TTY.WindowSize.Rows)),
			}
		}
	}

	err := transport.WriteMessage(reqBody, &protocol.RunRequest{
		Handle:     proto.String(handle),
		Path:       proto.String(spec.Path),
		Args:       spec.Args,
		Dir:        dir,
		Privileged: proto.Bool(spec.Privileged),
		User:       proto.String(spec.User),
		Tty:        tty,
		Rlimits: &protocol.ResourceLimits{
			As:         spec.Limits.As,
			Core:       spec.Limits.Core,
			Cpu:        spec.Limits.Cpu,
			Data:       spec.Limits.Data,
			Fsize:      spec.Limits.Fsize,
			Locks:      spec.Limits.Locks,
			Memlock:    spec.Limits.Memlock,
			Msgqueue:   spec.Limits.Msgqueue,
			Nice:       spec.Limits.Nice,
			Nofile:     spec.Limits.Nofile,
			Nproc:      spec.Limits.Nproc,
			Rss:        spec.Limits.Rss,
			Rtprio:     spec.Limits.Rtprio,
			Sigpending: spec.Limits.Sigpending,
			Stack:      spec.Limits.Stack,
		},
		Env: convertEnvironmentVariables(spec.Env),
	})
	if err != nil {
		return nil, err
	}

	conn, br, err := c.doHijack(
		routes.Run,
		reqBody,
		rata.Params{
			"handle": handle,
		},
		nil,
		"application/json",
	)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(br)

	firstResponse := &protocol.ProcessPayload{}
	err = decoder.Decode(firstResponse)
	if err != nil {
		return nil, err
	}

	p := newProcess(firstResponse.GetProcessId(), conn)

	go p.streamPayloads(decoder, processIO)

	return p, nil
}

func (c *connection) Attach(handle string, processID uint32, processIO api.ProcessIO) (api.Process, error) {
	reqBody := new(bytes.Buffer)

	err := transport.WriteMessage(reqBody, &protocol.AttachRequest{
		Handle:    proto.String(handle),
		ProcessId: proto.Uint32(processID),
	})
	if err != nil {
		return nil, err
	}

	conn, br, err := c.doHijack(
		routes.Attach,
		reqBody,
		rata.Params{
			"handle": handle,
			"pid":    fmt.Sprintf("%d", processID),
		},
		nil,
		"",
	)

	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(br)

	p := newProcess(processID, conn)

	go p.streamPayloads(decoder, processIO)

	return p, nil
}

func (c *connection) Kill(handle string, processID uint32) error {
	return c.do(
		routes.Kill,
		&protocol.SignalProcessRequest{
			Handle:    proto.String(handle),
			ProcessId: proto.Uint32(processID),
		},
		&protocol.SignalProcessResponse{},
		rata.Params{
			"handle": handle,
			"pid":    fmt.Sprintf("%d", processID),
		},
		nil,
	)
}

func (c *connection) NetIn(handle string, hostPort, containerPort uint32) (uint32, uint32, error) {
	res := &protocol.NetInResponse{}

	err := c.do(
		routes.NetIn,
		&protocol.NetInRequest{
			Handle:        proto.String(handle),
			HostPort:      proto.Uint32(hostPort),
			ContainerPort: proto.Uint32(containerPort),
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return 0, 0, err
	}

	return res.GetHostPort(), res.GetContainerPort(), nil
}

func (c *connection) NetOut(handle string, network string, port uint32) error {
	return c.do(
		routes.NetOut,
		&protocol.NetOutRequest{
			Handle:  proto.String(handle),
			Network: proto.String(network),
			Port:    proto.Uint32(port),
		},
		&protocol.NetOutResponse{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) GetProperty(handle string, name string) (string, error) {
	res := &protocol.GetPropertyResponse{}

	err := c.do(
		routes.GetProperty,
		&protocol.GetPropertyRequest{
			Handle: proto.String(handle),
			Key:    proto.String(name),
		},
		res,
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	if err != nil {
		return "", err
	}

	return res.GetValue(), nil
}

func (c *connection) SetProperty(handle string, name string, value string) error {
	res := &protocol.SetPropertyResponse{}

	err := c.do(
		routes.SetProperty,
		&protocol.SetPropertyRequest{
			Handle: proto.String(handle),
			Key:    proto.String(name),
			Value:  proto.String(value),
		},
		res,
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *connection) RemoveProperty(handle string, name string) error {
	res := &protocol.RemovePropertyResponse{}

	err := c.do(
		routes.RemoveProperty,
		&protocol.RemovePropertyRequest{
			Handle: proto.String(handle),
			Key:    proto.String(name),
		},
		res,
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *connection) LimitBandwidth(handle string, limits api.BandwidthLimits) (api.BandwidthLimits, error) {
	res := &protocol.LimitBandwidthResponse{}

	err := c.do(
		routes.LimitBandwidth,
		&protocol.LimitBandwidthRequest{
			Handle: proto.String(handle),
			Rate:   proto.Uint64(limits.RateInBytesPerSecond),
			Burst:  proto.Uint64(limits.BurstRateInBytesPerSecond),
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.BandwidthLimits{}, err
	}

	return api.BandwidthLimits{
		RateInBytesPerSecond:      res.GetRate(),
		BurstRateInBytesPerSecond: res.GetBurst(),
	}, nil
}

func (c *connection) CurrentBandwidthLimits(handle string) (api.BandwidthLimits, error) {
	res := &protocol.LimitBandwidthResponse{}

	err := c.do(
		routes.CurrentBandwidthLimits,
		nil,
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.BandwidthLimits{}, err
	}

	return api.BandwidthLimits{
		RateInBytesPerSecond:      res.GetRate(),
		BurstRateInBytesPerSecond: res.GetBurst(),
	}, nil
}

func (c *connection) LimitCPU(handle string, limits api.CPULimits) (api.CPULimits, error) {
	res := &protocol.LimitCpuResponse{}

	err := c.do(
		routes.LimitCPU,
		&protocol.LimitCpuRequest{
			Handle:        proto.String(handle),
			LimitInShares: proto.Uint64(limits.LimitInShares),
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.CPULimits{}, err
	}

	return api.CPULimits{
		LimitInShares: res.GetLimitInShares(),
	}, nil
}

func (c *connection) CurrentCPULimits(handle string) (api.CPULimits, error) {
	res := &protocol.LimitCpuResponse{}

	err := c.do(
		routes.CurrentCPULimits,
		nil,
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.CPULimits{}, err
	}

	return api.CPULimits{
		LimitInShares: res.GetLimitInShares(),
	}, nil
}

func (c *connection) LimitDisk(handle string, limits api.DiskLimits) (api.DiskLimits, error) {
	res := &protocol.LimitDiskResponse{}

	err := c.do(
		routes.LimitDisk,
		&protocol.LimitDiskRequest{
			Handle: proto.String(handle),

			BlockSoft: proto.Uint64(limits.BlockSoft),
			BlockHard: proto.Uint64(limits.BlockHard),

			InodeSoft: proto.Uint64(limits.InodeSoft),
			InodeHard: proto.Uint64(limits.InodeHard),

			ByteSoft: proto.Uint64(limits.ByteSoft),
			ByteHard: proto.Uint64(limits.ByteHard),
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.DiskLimits{}, err
	}

	return api.DiskLimits{
		BlockSoft: res.GetBlockSoft(),
		BlockHard: res.GetBlockHard(),

		InodeSoft: res.GetInodeSoft(),
		InodeHard: res.GetInodeHard(),

		ByteSoft: res.GetByteSoft(),
		ByteHard: res.GetByteHard(),
	}, nil
}

func (c *connection) CurrentDiskLimits(handle string) (api.DiskLimits, error) {
	res := &protocol.LimitDiskResponse{}

	err := c.do(
		routes.CurrentDiskLimits,
		nil,
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.DiskLimits{}, err
	}

	return api.DiskLimits{
		BlockSoft: res.GetBlockSoft(),
		BlockHard: res.GetBlockHard(),

		InodeSoft: res.GetInodeSoft(),
		InodeHard: res.GetInodeHard(),

		ByteSoft: res.GetByteSoft(),
		ByteHard: res.GetByteHard(),
	}, nil
}

func (c *connection) LimitMemory(handle string, limits api.MemoryLimits) (api.MemoryLimits, error) {
	res := &protocol.LimitMemoryResponse{}

	err := c.do(
		routes.LimitMemory,
		&protocol.LimitMemoryRequest{
			Handle:       proto.String(handle),
			LimitInBytes: proto.Uint64(limits.LimitInBytes),
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.MemoryLimits{}, err
	}

	return api.MemoryLimits{
		LimitInBytes: res.GetLimitInBytes(),
	}, nil
}

func (c *connection) CurrentMemoryLimits(handle string) (api.MemoryLimits, error) {
	res := &protocol.LimitMemoryResponse{}

	err := c.do(
		routes.CurrentMemoryLimits,
		nil,
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return api.MemoryLimits{}, err
	}

	return api.MemoryLimits{
		LimitInBytes: res.GetLimitInBytes(),
	}, nil
}

func (c *connection) StreamIn(handle string, dstPath string, reader io.Reader) error {
	body, err := c.doStream(
		routes.StreamIn,
		reader,
		rata.Params{
			"handle": handle,
		},
		url.Values{
			"destination": []string{dstPath},
		},
		"application/x-tar",
	)
	if err != nil {
		return err
	}

	return body.Close()
}

func (c *connection) StreamOut(handle string, srcPath string) (io.ReadCloser, error) {
	return c.doStream(
		routes.StreamOut,
		nil,
		rata.Params{
			"handle": handle,
		},
		url.Values{
			"source": []string{srcPath},
		},
		"",
	)
}

func (c *connection) List(filterProperties api.Properties) ([]string, error) {
	values := url.Values{}
	for name, val := range filterProperties {
		values[name] = []string{val}
	}

	res := &protocol.ListResponse{}

	err := c.do(
		routes.List,
		nil,
		res,
		nil,
		values,
	)
	if err != nil {
		return nil, err
	}

	return res.GetHandles(), nil
}

func (c *connection) Info(handle string) (api.ContainerInfo, error) {
	res := &protocol.InfoResponse{}

	err := c.do(routes.Info, nil, res, rata.Params{"handle": handle}, nil)
	if err != nil {
		return api.ContainerInfo{}, err
	}

	processIDs := []uint32{}
	for _, pid := range res.GetProcessIds() {
		processIDs = append(processIDs, uint32(pid))
	}

	properties := api.Properties{}
	for _, prop := range res.GetProperties() {
		properties[prop.GetKey()] = prop.GetValue()
	}

	mappedPorts := []api.PortMapping{}
	for _, mapping := range res.GetMappedPorts() {
		mappedPorts = append(mappedPorts, api.PortMapping{
			HostPort:      mapping.GetHostPort(),
			ContainerPort: mapping.GetContainerPort(),
		})
	}

	bandwidthStat := res.GetBandwidthStat()
	cpuStat := res.GetCpuStat()
	diskStat := res.GetDiskStat()
	memoryStat := res.GetMemoryStat()

	return api.ContainerInfo{
		State:  res.GetState(),
		Events: res.GetEvents(),

		HostIP:      res.GetHostIp(),
		ContainerIP: res.GetContainerIp(),
		ExternalIP:  res.GetExternalIp(),

		ContainerPath: res.GetContainerPath(),

		ProcessIDs: processIDs,

		Properties: properties,

		BandwidthStat: api.ContainerBandwidthStat{
			InRate:   bandwidthStat.GetInRate(),
			InBurst:  bandwidthStat.GetInBurst(),
			OutRate:  bandwidthStat.GetOutRate(),
			OutBurst: bandwidthStat.GetOutBurst(),
		},

		CPUStat: api.ContainerCPUStat{
			Usage:  cpuStat.GetUsage(),
			User:   cpuStat.GetUser(),
			System: cpuStat.GetSystem(),
		},

		DiskStat: api.ContainerDiskStat{
			BytesUsed:  diskStat.GetBytesUsed(),
			InodesUsed: diskStat.GetInodesUsed(),
		},

		MemoryStat: api.ContainerMemoryStat{
			Cache:                   memoryStat.GetCache(),
			Rss:                     memoryStat.GetRss(),
			MappedFile:              memoryStat.GetMappedFile(),
			Pgpgin:                  memoryStat.GetPgpgin(),
			Pgpgout:                 memoryStat.GetPgpgout(),
			Swap:                    memoryStat.GetSwap(),
			Pgfault:                 memoryStat.GetPgfault(),
			Pgmajfault:              memoryStat.GetPgmajfault(),
			InactiveAnon:            memoryStat.GetInactiveAnon(),
			ActiveAnon:              memoryStat.GetActiveAnon(),
			InactiveFile:            memoryStat.GetInactiveFile(),
			ActiveFile:              memoryStat.GetActiveFile(),
			Unevictable:             memoryStat.GetUnevictable(),
			HierarchicalMemoryLimit: memoryStat.GetHierarchicalMemoryLimit(),
			HierarchicalMemswLimit:  memoryStat.GetHierarchicalMemswLimit(),
			TotalCache:              memoryStat.GetTotalCache(),
			TotalRss:                memoryStat.GetTotalRss(),
			TotalMappedFile:         memoryStat.GetTotalMappedFile(),
			TotalPgpgin:             memoryStat.GetTotalPgpgin(),
			TotalPgpgout:            memoryStat.GetTotalPgpgout(),
			TotalSwap:               memoryStat.GetTotalSwap(),
			TotalPgfault:            memoryStat.GetTotalPgfault(),
			TotalPgmajfault:         memoryStat.GetTotalPgmajfault(),
			TotalInactiveAnon:       memoryStat.GetTotalInactiveAnon(),
			TotalActiveAnon:         memoryStat.GetTotalActiveAnon(),
			TotalInactiveFile:       memoryStat.GetTotalInactiveFile(),
			TotalActiveFile:         memoryStat.GetTotalActiveFile(),
			TotalUnevictable:        memoryStat.GetTotalUnevictable(),
		},

		MappedPorts: mappedPorts,
	}, nil
}

func convertEnvironmentVariables(environmentVariables []string) []*protocol.EnvironmentVariable {
	convertedEnvironmentVariables := []*protocol.EnvironmentVariable{}

	for _, env := range environmentVariables {
		segs := strings.SplitN(env, "=", 2)

		convertedEnvironmentVariable := &protocol.EnvironmentVariable{
			Key:   proto.String(segs[0]),
			Value: proto.String(segs[1]),
		}

		convertedEnvironmentVariables = append(
			convertedEnvironmentVariables,
			convertedEnvironmentVariable,
		)
	}

	return convertedEnvironmentVariables
}

func (c *connection) do(
	handler string,
	req, res proto.Message,
	params rata.Params,
	query url.Values,
) error {
	var body io.Reader

	if req != nil {
		buf := new(bytes.Buffer)

		err := transport.WriteMessage(buf, req)
		if err != nil {
			return err
		}

		body = buf
	}

	contentType := ""
	if req != nil {
		contentType = "application/json"
	}

	response, err := c.doStream(
		handler,
		body,
		params,
		query,
		contentType,
	)
	if err != nil {
		return err
	}

	defer response.Close()

	return json.NewDecoder(response).Decode(res)
}

func (c *connection) doStream(
	handler string,
	body io.Reader,
	params rata.Params,
	query url.Values,
	contentType string,
) (io.ReadCloser, error) {
	request, err := c.req.CreateRequest(handler, params, body)
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	if query != nil {
		request.URL.RawQuery = query.Encode()
	}

	httpResp, err := c.noKeepaliveClient.Do(request)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		errResponse, err := ioutil.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bad response: %s", httpResp.Status)
		}
		return nil, fmt.Errorf(string(errResponse))
	}

	return httpResp.Body, nil
}

func (c *connection) doHijack(
	handler string,
	body io.Reader,
	params rata.Params,
	query url.Values,
	contentType string,
) (net.Conn, *bufio.Reader, error) {
	request, err := c.req.CreateRequest(handler, params, body)
	if err != nil {
		return nil, nil, err
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	if query != nil {
		request.URL.RawQuery = query.Encode()
	}

	conn, err := c.dialer("tcp", "api") // net/addr don't matter here
	if err != nil {
		return nil, nil, err
	}

	client := httputil.NewClientConn(conn, nil)

	httpResp, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		httpResp.Body.Close()
		return nil, nil, fmt.Errorf("bad response: %s", httpResp.Status)
	}

	conn, br := client.Hijack()

	return conn, br, nil
}
