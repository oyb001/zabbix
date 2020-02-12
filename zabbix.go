package zabbix

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"
)

type (
	Params map[string]interface{}
)

type request struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	Auth    string      `json:"auth,omitempty"`
	Id      int32       `json:"id"`
}

type Response struct {
	Jsonrpc string      `json:"jsonrpc"`
	Error   *Error      `json:"error"`
	Result  interface{} `json:"result"`
	Id      int32       `json:"id"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d (%s): %s", e.Code, e.Message, e.Data)
}

type ExpectedOneResult int

func (e *ExpectedOneResult) Error() string {
	return fmt.Sprintf("Expected exactly one result, got %d.", *e)
}

type ExpectedMore struct {
	Expected int
	Got      int
}

func (e *ExpectedMore) Error() string {
	return fmt.Sprintf("Expected %d, got %d.", e.Expected, e.Got)
}

type API struct {
	Auth   string      // auth token, filled by Login()
	Logger *log.Logger // request/response logger, nil by default
	url    string
	c      http.Client
	id     int32
}

// Creates new API access object.
// Typical URL is http://host/api_jsonrpc.php or http://host/zabbix/api_jsonrpc.php.
// It also may contain HTTP basic auth username and password like
// http://username:password@host/api_jsonrpc.php.
func NewAPI(url string) (api *API) {
	return &API{url: url, c: http.Client{}}
}

// Allows one to use specific http.Client, for example with InsecureSkipVerify transport.
func (api *API) SetClient(c *http.Client) {
	api.c = *c
}

func (api *API) printf(format string, v ...interface{}) {
	if api.Logger != nil {
		api.Logger.Printf(format, v...)
	}
}

func (api *API) callBytes(method string, params interface{}) (b []byte, err error) {
	id := atomic.AddInt32(&api.id, 1)
	jsonobj := request{"2.0", method, params, api.Auth, id}
	b, err = json.Marshal(jsonobj)
	if err != nil {
		return
	}
	api.printf("Request (POST): %s", b)

	req, err := http.NewRequest("POST", api.url, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.ContentLength = int64(len(b))
	req.Header.Add("Content-Type", "application/json-rpc")
	req.Header.Add("User-Agent", "github.com/AlekSi/zabbix")

	res, err := api.c.Do(req)
	if err != nil {
		api.printf("Error   : %s", err)
		return
	}
	defer res.Body.Close()

	b, err = ioutil.ReadAll(res.Body)
	api.printf("Response (%d): %s", res.StatusCode, b)
	return
}

// Calls specified API method. Uses api.Auth if not empty.
// err is something network or marshaling related. Caller should inspect response.Error to get API error.
func (api *API) Call(method string, params interface{}) (response Response, err error) {
	b, err := api.callBytes(method, params)
	if err == nil {
		err = json.Unmarshal(b, &response)
	}
	return
}

// Uses Call() and then sets err to response.Error if former is nil and latter is not.
func (api *API) CallWithError(method string, params interface{}) (response Response, err error) {
	response, err = api.Call(method, params)
	if err == nil && response.Error != nil {
		err = response.Error
	}
	return
}

// Calls "user.login" API method and fills api.Auth field.
// This method modifies API structure and should not be called concurrently with other methods.
func (api *API) Login(user, password string) (auth string, err error) {
	params := map[string]string{"user": user, "password": password}
	response, err := api.CallWithError("user.login", params)
	if err != nil {
		return
	}

	auth = response.Result.(string)
	api.Auth = auth
	return
}

// Calls "APIInfo.version" API method.
// This method temporary modifies API structure and should not be called concurrently with other methods.
func (api *API) Version() (v string, err error) {
	// temporary remove auth for this method to succeed
	// https://www.zabbix.com/documentation/2.2/manual/appendix/api/apiinfo/version
	auth := api.Auth
	api.Auth = ""
	response, err := api.CallWithError("APIInfo.version", Params{})
	api.Auth = auth

	// despite what documentation says, Zabbix 2.2 requires auth, so we try again
	if e, ok := err.(*Error); ok && e.Code == -32602 {
		response, err = api.CallWithError("APIInfo.version", Params{})
	}
	if err != nil {
		return
	}

	v = response.Result.(string)
	return
}

var ErrBadHeader = errors.New("bad header")

type Response1 struct {
	Response  string `json:"response"` // "success" on success
	Info      string `json:"info"`     // String like "Processed 2 Failed 1 Total 3 Seconds spent 0.000034"
	Processed int                      // Filled by parsing Info
	Failed    int                      // Filled by parsing Info
}

var infoRE = regexp.MustCompile(`(?i)Processed:? (\d+);? Failed:? (\d+)`)

// Send DataItems to Zabbix server and wait for response.
// Returns encountered fatal error like I/O and marshalling/unmarshalling.
// Caller should inspect response (and in some situations also Zabbix server log)
// to check if all items are accepted.
func Send(addr *net.TCPAddr, di DataItems) (res *Response1, err error) {
	b, err := di.Marshal()
	if err != nil {
		return
	}

	// Zabbix doesn't support persistent connections, so open/close it every time.
	conn, err := net.DialTCP(addr.Network(), nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()

	_, err = conn.Write(b)
	if err != nil {
		return
	}

	buf := make([]byte, 8)
	_, err = io.ReadFull(conn, buf[:5])
	if err != nil {
		return
	}
	if !bytes.Equal(buf[:5], header) {
		err = ErrBadHeader
		return
	}

	_, err = io.ReadFull(conn, buf)
	if err != nil {
		return
	}
	var datalen uint64
	err = binary.Read(bytes.NewBuffer(buf), binary.LittleEndian, &datalen)
	if err != nil {
		err = ErrBadHeader
		return
	}

	buf = make([]byte, datalen)
	_, err = io.ReadFull(conn, buf)
	if err != nil {
		return
	}

	res = new(Response1)
	err = json.Unmarshal(buf, res)
	if err == nil {
		m := infoRE.FindStringSubmatch(res.Info)
		if len(m) == 3 {
			p, _ := strconv.Atoi(m[1])
			f, _ := strconv.Atoi(m[2])
			res.Processed = p
			res.Failed = f
		}
	}
	return
}

var (
	header = []byte("ZBXD\x01")
)

// Converts value to format accepted by Zabbix server.
// It uses "%.6f" format for floats,
// and fmt.Sprint() (which will try String() and Error() methods) for other types.
// Keep in mind that Zabbix doesn't support negative integers, use floats instead.
func ConvertValue(i interface{}) string {
	switch v := i.(type) {
	case float32:
		return fmt.Sprintf("%.6f", v)
	case float64:
		return fmt.Sprintf("%.6f", v)
	default:
		return fmt.Sprint(v)
	}
	panic("not reached")
}

type DataItem struct {
	Hostname  string `json:"host"`
	Key       string `json:"key"`
	Timestamp int64  `json:"clock,omitempty"` // UNIX timestamp, 0 is ignored
	Value     string `json:"value"`           // Use ConvertValue() to fill
}
type DataItems []DataItem

func MakeDataItems(kv map[string]interface{}, hostname string) DataItems {
	di := make(DataItems, len(kv))
	i := 0
	for k, v := range kv {
		di[i] = DataItem{hostname, k, 0, ConvertValue(v)}
		i++
	}

	return di
}
func (di DataItems) Marshal() (b []byte, err error) {
	d, err := json.Marshal(di)
	if err == nil {
		// the order of fields in this "JSON" is important - request should be before data
		now := fmt.Sprint(time.Now().Unix())
		datalen := uint64(len(d) + len(now) + 42) // 32 + d + 9 + now + 1
		b = make([]byte, 0, datalen+13)           // datalen + 5 + 8
		buf := bytes.NewBuffer(b)
		buf.Write(header)                                     // 5
		err = binary.Write(buf, binary.LittleEndian, datalen) // 8
		buf.WriteString(`{"request":"sender data","data":`)   // 32
		buf.Write(d)                                          // d
		buf.WriteString(`,"clock":`)                          // 9
		buf.WriteString(now)                                  // now
		buf.WriteByte('}')                                    // 1
		b = buf.Bytes()
	}
	return
}


