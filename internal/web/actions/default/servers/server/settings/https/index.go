package https

import (
	"encoding/json"
	"errors"
	"github.com/TeaOSLab/EdgeAdmin/internal/oplogs"
	"github.com/TeaOSLab/EdgeAdmin/internal/web/actions/actionutils"
	"github.com/TeaOSLab/EdgeAdmin/internal/web/actions/default/servers/serverutils"
	"github.com/TeaOSLab/EdgeCommon/pkg/nodeconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs"
	"github.com/TeaOSLab/EdgeCommon/pkg/serverconfigs/sslconfigs"
	"github.com/iwind/TeaGo/actions"
	"github.com/iwind/TeaGo/lists"
	"github.com/iwind/TeaGo/maps"
	"github.com/iwind/TeaGo/types"
	"regexp"
)

type IndexAction struct {
	actionutils.ParentAction
}

func (this *IndexAction) Init() {
	this.Nav("", "setting", "index")
	this.SecondMenu("https")
}

func (this *IndexAction) RunGet(params struct {
	ServerId int64
}) {
	server, _, isOk := serverutils.FindServer(this.Parent(), params.ServerId)
	if !isOk {
		return
	}
	var httpsConfig = &serverconfigs.HTTPSProtocolConfig{}
	if len(server.HttpsJSON) > 0 {
		err := json.Unmarshal(server.HttpsJSON, httpsConfig)
		if err != nil {
			this.ErrorPage(err)
			return
		}
	} else {
		httpsConfig.IsOn = true
	}

	_ = httpsConfig.Init(nil)
	var httpsPorts = httpsConfig.AllPorts()

	// 检查http和https端口冲突
	var conflictingPorts = []int{}
	if len(server.HttpJSON) > 0 {
		var httpConfig = &serverconfigs.HTTPProtocolConfig{}
		err := json.Unmarshal(server.HttpJSON, httpConfig)
		if err != nil {
			this.ErrorPage(err)
			return
		}
		_ = httpConfig.Init()
		for _, port := range httpConfig.AllPorts() {
			if lists.ContainsInt(httpsPorts, port) {
				conflictingPorts = append(conflictingPorts, port)
			}
		}
	}
	this.Data["conflictingPorts"] = conflictingPorts

	var sslPolicy *sslconfigs.SSLPolicy
	if httpsConfig.SSLPolicyRef != nil && httpsConfig.SSLPolicyRef.SSLPolicyId > 0 {
		sslPolicyConfigResp, err := this.RPC().SSLPolicyRPC().FindEnabledSSLPolicyConfig(this.AdminContext(), &pb.FindEnabledSSLPolicyConfigRequest{
			SslPolicyId: httpsConfig.SSLPolicyRef.SSLPolicyId,
			IgnoreData:  true,
		})
		if err != nil {
			this.ErrorPage(err)
			return
		}
		var sslPolicyConfigJSON = sslPolicyConfigResp.SslPolicyJSON
		if len(sslPolicyConfigJSON) > 0 {
			sslPolicy = &sslconfigs.SSLPolicy{}
			err = json.Unmarshal(sslPolicyConfigJSON, sslPolicy)
			if err != nil {
				this.ErrorPage(err)
				return
			}
		}
	}

	// 当前集群是否支持HTTP/3
	// TODO 检查当前服务所属用户是否支持HTTP/3
	if server.NodeCluster == nil {
		this.ErrorPage(errors.New("no node cluster for the server"))
		return
	}
	http3PolicyResp, err := this.RPC().NodeClusterRPC().FindNodeClusterHTTP3Policy(this.AdminContext(), &pb.FindNodeClusterHTTP3PolicyRequest{NodeClusterId: server.NodeCluster.Id})
	if err != nil {
		this.ErrorPage(err)
		return
	}
	var supportsHTTP3 = false
	if len(http3PolicyResp.Http3PolicyJSON) > 0 {
		var http3Policy = nodeconfigs.NewHTTP3Policy()
		err = json.Unmarshal(http3PolicyResp.Http3PolicyJSON, http3Policy)
		if err != nil {
			this.ErrorPage(err)
			return
		}
		supportsHTTP3 = http3Policy.IsOn
	}

	this.Data["serverType"] = server.Type
	this.Data["httpsConfig"] = maps.Map{
		"isOn":          httpsConfig.IsOn,
		"addresses":     httpsConfig.Listen,
		"sslPolicy":     sslPolicy,
		"supportsHTTP3": supportsHTTP3,
	}

	this.Show()
}

func (this *IndexAction) RunPost(params struct {
	ServerId  int64
	IsOn      bool
	Addresses string

	SslPolicyJSON []byte

	Must *actions.Must
}) {
	// 记录日志
	defer this.CreateLog(oplogs.LevelInfo, "修改服务 %d 的HTTPS设置", params.ServerId)

	var addresses = []*serverconfigs.NetworkAddressConfig{}
	err := json.Unmarshal([]byte(params.Addresses), &addresses)
	if err != nil {
		this.Fail("端口地址解析失败：" + err.Error())
	}

	// 如果启用HTTPS时没有填写端口，则默认为443
	if params.IsOn && len(addresses) == 0 {
		addresses = []*serverconfigs.NetworkAddressConfig{
			{
				Protocol:  serverconfigs.ProtocolHTTPS,
				PortRange: "443",
			},
		}
	}

	// 检查端口地址是否正确
	for _, addr := range addresses {
		err = addr.Init()
		if err != nil {
			this.Fail("绑定端口校验失败：" + err.Error())
		}

		if regexp.MustCompile(`^\d+$`).MatchString(addr.PortRange) {
			port := types.Int(addr.PortRange)
			if port > 65535 {
				this.Fail("绑定的端口地址不能大于65535")
			}
			if port == 80 {
				this.Fail("端口80通常是HTTP的端口，不能用在HTTPS上")
			}
		}
	}

	// 校验SSL
	var sslPolicyId = int64(0)
	if params.SslPolicyJSON != nil {
		sslPolicy := &sslconfigs.SSLPolicy{}
		err = json.Unmarshal(params.SslPolicyJSON, sslPolicy)
		if err != nil {
			this.ErrorPage(errors.New("解析SSL配置时发生了错误：" + err.Error()))
			return
		}

		sslPolicyId = sslPolicy.Id

		certsJSON, err := json.Marshal(sslPolicy.CertRefs)
		if err != nil {
			this.ErrorPage(err)
			return
		}

		hstsJSON, err := json.Marshal(sslPolicy.HSTS)
		if err != nil {
			this.ErrorPage(err)
			return
		}

		clientCACertsJSON, err := json.Marshal(sslPolicy.ClientCARefs)
		if err != nil {
			this.ErrorPage(err)
			return
		}

		if sslPolicyId > 0 {
			_, err := this.RPC().SSLPolicyRPC().UpdateSSLPolicy(this.AdminContext(), &pb.UpdateSSLPolicyRequest{
				SslPolicyId:       sslPolicyId,
				Http2Enabled:      sslPolicy.HTTP2Enabled,
				Http3Enabled:      sslPolicy.HTTP3Enabled,
				MinVersion:        sslPolicy.MinVersion,
				SslCertsJSON:      certsJSON,
				HstsJSON:          hstsJSON,
				OcspIsOn:          sslPolicy.OCSPIsOn,
				ClientAuthType:    types.Int32(sslPolicy.ClientAuthType),
				ClientCACertsJSON: clientCACertsJSON,
				CipherSuitesIsOn:  sslPolicy.CipherSuitesIsOn,
				CipherSuites:      sslPolicy.CipherSuites,
			})
			if err != nil {
				this.ErrorPage(err)
				return
			}
		} else {
			resp, err := this.RPC().SSLPolicyRPC().CreateSSLPolicy(this.AdminContext(), &pb.CreateSSLPolicyRequest{
				Http2Enabled:      sslPolicy.HTTP2Enabled,
				Http3Enabled:      sslPolicy.HTTP3Enabled,
				MinVersion:        sslPolicy.MinVersion,
				SslCertsJSON:      certsJSON,
				HstsJSON:          hstsJSON,
				OcspIsOn:          sslPolicy.OCSPIsOn,
				ClientAuthType:    types.Int32(sslPolicy.ClientAuthType),
				ClientCACertsJSON: clientCACertsJSON,
				CipherSuitesIsOn:  sslPolicy.CipherSuitesIsOn,
				CipherSuites:      sslPolicy.CipherSuites,
			})
			if err != nil {
				this.ErrorPage(err)
				return
			}
			sslPolicyId = resp.SslPolicyId
		}
	}

	server, _, isOk := serverutils.FindServer(this.Parent(), params.ServerId)
	if !isOk {
		return
	}
	var httpsConfig = &serverconfigs.HTTPSProtocolConfig{}
	if len(server.HttpsJSON) > 0 {
		err = json.Unmarshal(server.HttpsJSON, httpsConfig)
		if err != nil {
			this.ErrorPage(err)
			return
		}
	}

	httpsConfig.SSLPolicy = nil
	httpsConfig.SSLPolicyRef = &sslconfigs.SSLPolicyRef{
		IsOn:        true,
		SSLPolicyId: sslPolicyId,
	}
	httpsConfig.IsOn = params.IsOn
	httpsConfig.Listen = addresses
	configData, err := json.Marshal(httpsConfig)
	if err != nil {
		this.ErrorPage(err)
		return
	}

	_, err = this.RPC().ServerRPC().UpdateServerHTTPS(this.AdminContext(), &pb.UpdateServerHTTPSRequest{
		ServerId:  params.ServerId,
		HttpsJSON: configData,
	})
	if err != nil {
		this.ErrorPage(err)
		return
	}

	this.Success()
}
